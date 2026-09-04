//go:build darwin || linux

package main

import "syscall"

// O_NOFOLLOW closes the final-component symlink race and O_NONBLOCK prevents
// an attacker from replacing a validated path with a blocking special file.
const secureOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
