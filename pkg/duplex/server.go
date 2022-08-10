/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package duplex

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"strings"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// grpcHandlerFunc routes inbound requests to either the passed gRPC server or
// the http handler based on the request content type.
// See also, https://grpc-ecosystem.github.io/grpc-gateway/
// This is based on: https://github.com/philips/grpc-gateway-example/issues/22#issuecomment-490733965
func grpcHandlerFunc(grpcServer *grpc.Server, httpHandler http.Handler) http.Handler {
	return h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.Contains(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
		} else {
			httpHandler.ServeHTTP(w, r)
		}
	}), &http2.Server{})
}

// Duplex is a wrapper for the gRPC server, gRPC HTTP Gateway MUX and options.
type Duplex struct {
	Server      *grpc.Server
	MUX         *runtime.ServeMux
	Loopback    string
	Port        int
	DialOptions []grpc.DialOption
}

type RegisterHandlerFromEndpointFn func(ctx context.Context, mux *runtime.ServeMux, endpoint string, opts []grpc.DialOption) error

// New creates a Duplex gRPC server / gRPC HTTP Gateway. New takes in options
// for `grpc.NewServer`, typed `grpc.ServerOption`, and `runtime.NewServeMux`,
// typed `runtime.ServeMuxOption`. Unknown opts will cause a panic.
func New(port int, opts ...interface{}) *Duplex {
	// Split out the options into their types.
	var gOpts []grpc.ServerOption
	var mOpts []runtime.ServeMuxOption
	for _, o := range opts {
		switch opt := o.(type) {
		case grpc.ServerOption:
			gOpts = append(gOpts, opt)
		case runtime.ServeMuxOption:
			mOpts = append(mOpts, opt)
		default:
			panic(fmt.Errorf("unknown type: %T", o))
		}
	}

	// Create the Duplex Server.
	d := &Duplex{
		Server: grpc.NewServer(gOpts...),
		MUX:    runtime.NewServeMux(mOpts...),
		// The REST gateway translates the json to grpc and then dispatches to
		// the appropriate method on this address, so we loopback to ourselves.
		Loopback:    fmt.Sprintf("localhost:%d", port),
		Port:        port,
		DialOptions: []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
	}
	return d
}

// RegisterHandler is a helper registration handler to call the passed in
// `RegisterHandlerFromEndpointFn` with the correct options after `d.Server`
// has been registered with the implementation. Use like:
// ```go
//	pb.Register<Type>Server(d.Server, impl.New<TypeImpl>())
//	if err := d.RegisterHandler(ctx, pb.Register<Type>HandlerFromEndpoint); err != nil {
//		log.Panicf("Failed to register gateway endpoint: %v", err)
//	}
// ```
func (d *Duplex) RegisterHandler(ctx context.Context, fn RegisterHandlerFromEndpointFn) error {
	return fn(ctx, d.MUX, d.Loopback, d.DialOptions)
}

// ListenAndServe starts both the gRPC server and HTTP Gateway MUX.
// Note: This call is blocking.
func (d *Duplex) ListenAndServe(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", d.Port)
	return http.ListenAndServe(addr, grpcHandlerFunc(d.Server, d.MUX))
}

// RegisterListenAndServe initializes Prometheus metrics and starts a HTTP
// /metrics endpoint for exporting Prometheus metrics in the background.
// Call this *after* all services have been registered.
func (d *Duplex) RegisterListenAndServeMetrics(port int, enablePprof bool) {
	grpc_prometheus.Register(d.Server)

	go func(mport int) {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())

		if enablePprof {
			// pprof handles
			mux.HandleFunc("/debug/pprof/", pprof.Index)
			mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
			mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
			mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
			mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
			mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
			mux.Handle("/debug/pprof/block", pprof.Handler("block"))
			mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
			mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
			mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
			mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))

			log.Println("registering handle for /debug/pprof")
		}

		if err := http.ListenAndServe(fmt.Sprintf(":%d", mport), mux); err != nil {
			log.Fatalf("listen and server for http /metrics = %v", err)
		}
	}(port)
}
