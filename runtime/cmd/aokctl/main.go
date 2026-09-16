package main

import (
	aok "aok/runtime"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	socket := flag.String("socket", "", "supervisor control socket")
	flag.Parse()
	if flag.NArg() < 1 || flag.NArg() > 2 {
		fmt.Fprintln(os.Stderr, "usage: aokctl -socket PATH METHOD [JSON_PARAMS]")
		os.Exit(2)
	}
	params := json.RawMessage(`{}`)
	if flag.NArg() == 2 {
		params = json.RawMessage(flag.Arg(1))
	}
	if !json.Valid(params) {
		fmt.Fprintln(os.Stderr, "invalid JSON params")
		os.Exit(2)
	}
	c, err := aok.DialControl(*socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var result json.RawMessage
	if err = c.Call(ctx, flag.Arg(0), params, &result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(result))
}
