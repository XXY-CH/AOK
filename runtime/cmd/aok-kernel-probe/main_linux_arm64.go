//go:build linux && arm64

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	aok "aok/runtime"
	"aok/runtime/kernelbridge"
)

func network() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	request := func(cmd uintptr, ip string, flags uint16) error {
		var req [40]byte
		copy(req[:16], "eth0")
		if ip != "" {
			*(*uint16)(unsafe.Pointer(&req[16])) = syscall.AF_INET
			copy(req[20:24], net.ParseIP(ip).To4())
		} else {
			*(*uint16)(unsafe.Pointer(&req[16])) = flags
		}
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), cmd, uintptr(unsafe.Pointer(&req[0])))
		if e != 0 {
			return e
		}
		return nil
	}
	if err := request(syscall.SIOCSIFADDR, "10.0.2.15", 0); err != nil {
		return err
	}
	if err := request(syscall.SIOCSIFNETMASK, "255.255.255.0", 0); err != nil {
		return err
	}
	return request(syscall.SIOCSIFFLAGS, "", syscall.IFF_UP|syscall.IFF_RUNNING)
}

func runProvider(root *kernelbridge.Root, provider aok.Provider, prompt string) error {
	spec := kernelbridge.CapabilitySpec{Rights: kernelbridge.All, TokenLimit: 2048, Taint: 1, ExportMask: 1}
	cap, err := root.Create(spec)
	if err != nil {
		return err
	}
	defer cap.Close()
	child, err := cap.Derive(spec)
	if err != nil {
		return err
	}
	defer child.Close()
	client, backend, err := child.Session()
	if err != nil {
		return err
	}
	defer client.Close()
	defer backend.Close()
	if err := client.Submit(kernelbridge.Record{Sequence: 1, InputTokens: 1024, OutputTokens: 64, Data: []byte(prompt)}); err != nil {
		return err
	}
	req, err := backend.Take()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	text, usage, err := provider.Complete(ctx, string(req.Data))
	if err != nil {
		return err
	}
	if len(text) == 0 || usage.InputTokens == 0 || usage.OutputTokens == 0 {
		return errors.New("backend produced empty completion or usage")
	}
	if err := backend.Complete(kernelbridge.Record{Sequence: req.Sequence, InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, Data: []byte(text)}); err != nil {
		return err
	}
	result, err := client.Result()
	if err != nil {
		return err
	}
	if result.TokensUsed != usage.InputTokens+usage.OutputTokens || result.Taint != 1 || string(result.Data) != text {
		return errors.New("kernel result mismatch")
	}
	if _, err := client.Export(); err != nil {
		return err
	}
	if err := client.Submit(kernelbridge.Record{Sequence: 2, InputTokens: 4096}); !errors.Is(err, syscall.EDQUOT) {
		return fmt.Errorf("budget bypass: %v", err)
	}
	if err := cap.Revoke(); err != nil {
		return err
	}
	if err := child.Check(); !errors.Is(err, syscall.EACCES) {
		return fmt.Errorf("revocation bypass: %v", err)
	}
	if _, err := client.Result(); !errors.Is(err, syscall.EACCES) {
		return fmt.Errorf("session revocation bypass: %v", err)
	}
	fmt.Printf("AOK_KERNEL_PROVIDER=%s input=%d output=%d used=%d text=%q\n", provider.Name(), usage.InputTokens, usage.OutputTokens, result.TokensUsed, text)
	return nil
}

func run() error {
	if os.Getpid() != 1 {
		return errors.New("kernel probe must run as guest PID1")
	}
	for _, mount := range []struct{ path, fs string }{{"/proc", "proc"}, {"/sys", "sysfs"}, {"/dev", "devtmpfs"}} {
		if err := os.MkdirAll(mount.path, 0755); err != nil {
			return err
		}
		if err := syscall.Mount(mount.fs, mount.path, mount.fs, 0, ""); err != nil {
			return err
		}
	}
	root, err := kernelbridge.Bootstrap()
	if err != nil {
		return err
	}
	defer root.Close()
	if err := runProvider(root, aok.EchoProvider{}, "kernel echo"); err != nil {
		return err
	}
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return err
	}
	endpoint := ""
	for _, field := range strings.Fields(string(cmdline)) {
		if strings.HasPrefix(field, "aok.llama=") {
			endpoint = strings.TrimPrefix(field, "aok.llama=")
		}
	}
	if endpoint != "" {
		if err := network(); err != nil {
			return fmt.Errorf("network: %w", err)
		}
		provider, err := aok.NewLlamaProvider(aok.LlamaConfig{Endpoint: endpoint, MaxTokens: 32, Timeout: 45 * time.Second})
		if err != nil {
			return err
		}
		if err := runProvider(root, provider, "Once upon a time, a little girl found a"); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	err := run()
	if err != nil {
		fmt.Printf("AOK_KERNEL_PROBE=fail error=%v\n", err)
	} else {
		fmt.Println("AOK_KERNEL_PROBE=pass")
	}
	if os.Getpid() == 1 {
		syscall.Sync()
		_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
		select {}
	}
	if err != nil {
		os.Exit(1)
	}
}
