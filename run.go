package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	localgateway "github.com/HuanyuMark/localgateway-go/client"
	"github.com/HuanyuMark/localgateway-go/generated/gatewayapi"
)

const (
	initialRegisterBackoff = 100 * time.Millisecond
	maxRegisterBackoff     = 2 * time.Second
	defaultShutdownGrace   = 10 * time.Second
	defaultServeGrace      = 5 * time.Second
)

// Start 绑定 listener、开始服务并完成首次注册（失败时在 RegisterRetry 窗口内退避重试）。
//
// Start 返回后服务已在运行；调用方需要自行在适当时候调用 Shutdown。多数程序
// 直接使用 Run 即可。
func (p *Provider) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return ErrAlreadyStarted
	}
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	p.started = true
	p.mu.Unlock()

	if err := ctx.Err(); err != nil {
		p.failStart()
		return err
	}

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p.cfg.Port)))
	if err != nil {
		p.failStart()
		return fmt.Errorf("provider: listen on 127.0.0.1:%d: %w", p.cfg.Port, err)
	}

	client := p.cfg.GatewayClient
	if client == nil {
		client, err = localgateway.New(localgateway.Options{})
		if err != nil {
			_ = listener.Close()
			p.failStart()
			return fmt.Errorf("provider: create gateway client: %w", err)
		}
	}

	serveCtx, serveCancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)

	p.mu.Lock()
	p.listener = listener
	p.port = listener.Addr().(*net.TCPAddr).Port
	p.client = client
	p.serveCancel = serveCancel
	p.serveErr = serveErr
	p.mu.Unlock()

	go func() { serveErr <- p.serve(serveCtx, listener) }()

	if err := p.register(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultServeGrace)
		_ = p.stopServe(cleanupCtx)
		cancel()
		p.failStart()
		return err
	}
	return nil
}

// Run 启动服务、注册，并阻塞到 ctx 取消后优雅退出。
//
// 退出顺序：按需 DELETE 注销 → 停止 HTTP 服务。自定义 Serve 适配器会先收到
// ctx 取消信号。
func (p *Provider) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := p.Start(ctx); err != nil {
		return err
	}

	p.mu.Lock()
	serveErr := p.serveErr
	p.mu.Unlock()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		p.mu.Lock()
		p.serveExited = true
		p.mu.Unlock()
		if err != nil {
			_ = p.Shutdown(context.Background())
			return fmt.Errorf("provider: serve: %w", err)
		}
		_ = p.Shutdown(context.Background())
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownGrace)
	defer cancel()
	return p.Shutdown(shutdownCtx)
}

// Shutdown 按配置注销并停止服务。可重复调用。
func (p *Provider) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if !p.started || p.closed {
		p.mu.Unlock()
		return nil
	}
	registered := p.registered
	client := p.client
	p.mu.Unlock()

	var failures []error
	if registered && p.cfg.UnregisterOnShutdown && client != nil {
		if err := client.Delete(ctx, p.cfg.Name); err != nil {
			failures = append(failures, fmt.Errorf("provider: unregister %s: %w", p.cfg.Name, err))
		}
	}
	if err := p.stopServe(ctx); err != nil && !errors.Is(err, context.Canceled) {
		failures = append(failures, fmt.Errorf("provider: stop serving: %w", err))
	}

	p.mu.Lock()
	p.closed = true
	p.started = false
	p.registered = false
	p.listener = nil
	p.serveCancel = nil
	p.mu.Unlock()

	return errors.Join(failures...)
}

// register 在 RegisterRetry 窗口内按指数退避注册；不可重试的错误立即返回。
func (p *Provider) register(ctx context.Context) error {
	deadline := time.Now().Add(p.cfg.RegisterRetry)
	delay := initialRegisterBackoff

	for {
		if err := p.takeServeError(); err != nil {
			return err
		}

		_, err := p.client.Register(ctx, p.cfg.Name, localgateway.RegisterOptions{
			Port:        p.Port(),
			Version:     p.cfg.Version,
			Probe:       localgateway.Probe{Path: p.cfg.ProbePath, TimeoutMs: localgateway.PtrInt32(int32(p.cfg.ProbeTimeoutMs))},
			APIInfo:     p.spec,
			ProjectRoot: p.cfg.ProjectRoot,
			NotifyURL:   fmt.Sprintf("http://127.0.0.1:%d%s", p.Port(), p.cfg.NotifyPath),
			CORS:        p.cfg.CORS,
		})
		if err == nil {
			p.mu.Lock()
			p.registered = true
			p.mu.Unlock()
			return nil
		}
		if !registrationRetryable(err) {
			return fmt.Errorf("provider: register %s: %w", p.cfg.Name, err)
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("provider: register %s: %w", p.cfg.Name, err)
		}
		wait := delay
		if wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case err := <-p.serveChannel():
			timer.Stop()
			p.markServeExited()
			if err == nil {
				return errors.New("provider: serve stopped before registration completed")
			}
			return fmt.Errorf("provider: serve: %w", err)
		case <-timer.C:
		}
		if delay < maxRegisterBackoff {
			delay *= 2
			if delay > maxRegisterBackoff {
				delay = maxRegisterBackoff
			}
		}
	}
}

func (p *Provider) serveChannel() chan error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.serveErr
}

func (p *Provider) markServeExited() {
	p.mu.Lock()
	p.serveExited = true
	p.mu.Unlock()
}

func (p *Provider) takeServeError() error {
	p.mu.Lock()
	serveErr := p.serveErr
	exited := p.serveExited
	p.mu.Unlock()
	if exited || serveErr == nil {
		return nil
	}
	select {
	case err := <-serveErr:
		p.markServeExited()
		if err == nil {
			return errors.New("provider: serve stopped before registration completed")
		}
		return fmt.Errorf("provider: serve: %w", err)
	default:
		return nil
	}
}

// stopServe 取消自定义适配器或内置服务器的上下文，并等待其退出。
func (p *Provider) stopServe(ctx context.Context) error {
	p.mu.Lock()
	cancel := p.serveCancel
	serveErr := p.serveErr
	listener := p.listener
	exited := p.serveExited
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if exited || serveErr == nil {
		if listener != nil {
			_ = listener.Close()
		}
		return nil
	}

	select {
	case err := <-serveErr:
		p.markServeExited()
		if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		if listener != nil {
			_ = listener.Close()
		}
		return ctx.Err()
	}
}

func (p *Provider) failStart() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listener != nil {
		_ = p.listener.Close()
	}
	p.closed = true
	p.started = false
	p.listener = nil
	p.serveCancel = nil
	p.serveErr = nil
	p.port = 0
}

// registrationRetryable 只对传输错误与网关暂时不可用（5xx/429/408）重试；
// 鉴权与参数错误会一直失败，立即返回。
func registrationRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *gatewayapi.GenericOpenAPIError
	if errors.As(err, &apiErr) {
		status, ok := leadingStatusCode(apiErr.Error())
		if !ok {
			return false
		}
		return status >= 500 || status == http.StatusTooManyRequests || status == http.StatusRequestTimeout
	}
	for _, sentinel := range []error{
		localgateway.ErrInvalidName,
		localgateway.ErrInvalidPort,
		localgateway.ErrInvalidVersion,
		localgateway.ErrInvalidProbe,
		localgateway.ErrInvalidCORS,
		localgateway.ErrEmptyPatch,
		localgateway.ErrInvalidBaseURL,
	} {
		if errors.Is(err, sentinel) {
			return false
		}
	}
	return true
}

func leadingStatusCode(message string) (int, bool) {
	fields := strings.Fields(message)
	if len(fields) == 0 {
		return 0, false
	}
	status, err := strconv.Atoi(fields[0])
	if err != nil || status < 100 || status > 599 {
		return 0, false
	}
	return status, true
}
