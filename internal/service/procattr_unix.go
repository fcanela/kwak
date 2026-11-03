//go:build !windows

package service

import "syscall"

func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

func childAttr() *syscall.SysProcAttr { return nil }
