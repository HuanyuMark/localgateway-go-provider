# localgateway-go-provider

Go provider lifecycle library for LocalGateway. It binds a loopback listener,
serves the built-in probe and restart notification endpoints, registers with the
gateway, retries with backoff and shuts down gracefully.

## Usage

```powershell
go get github.com/HuanyuMark/localgateway-go-provider@v0.1.0
```

```go
import "github.com/HuanyuMark/localgateway-go-provider"

p, err := provider.New(provider.Config{
    Name:    "my-service",
    Version: "1.0.0",
    Port:    0,
    Handler: mux,
})
if err != nil {
    return err
}
return p.Run(ctx)
```

`localgateway-go-provider` requires `github.com/HuanyuMark/localgateway-go`; `go get`
resolves both modules.