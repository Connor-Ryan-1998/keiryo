package main

import (
	"os"
	"syscall"
)

func dispatchAgent(binary string, args []string) error {
	return syscall.Exec(binary, append([]string{binary}, args...), os.Environ())
}
