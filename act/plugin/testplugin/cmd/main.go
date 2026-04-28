package main

import (
	"fmt"
	"net"
	"os"

	"code.forgejo.org/forgejo/runner/v12/act/plugin/testplugin"
	"google.golang.org/grpc"
)

func main() {
	addr := os.Getenv("PLUGIN_LISTEN_ADDR")
	if addr == "" {
		addr = "localhost:0"
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}

	srv := grpc.NewServer()
	testplugin.New().Register(srv)

	fmt.Fprintln(os.Stdout, lis.Addr().String())

	if err := srv.Serve(lis); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}
