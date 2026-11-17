package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// ServeFunc 在 listener 上阻塞服务，ctx 取消后优雅停止。
//
// handler 已由调用方通过闭包持有（例如 ServeNetHTTP(mux)）；返回 nil 表示正常退出。
type ServeFunc func(ctx context.Context, listener net.Listener) error

// ServeNetHTTP 是 net/http（含 humago、gin、echo 等实现 http.Handler 的框架）
// 的默认启动函数。
func ServeNetHTTP(handler http.Handler) ServeFunc {
	return func(ctx context.Context, listener net.Listener) error {
		server := &http.Server{Handler: handler}
		serveErr := make(chan error, 1)
		go func() { serveErr <- server.Serve(listener) }()

		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultServeGrace)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				_ = server.Close()
			}
			err := <-serveErr
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case err := <-serveErr:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		}
	}
}
