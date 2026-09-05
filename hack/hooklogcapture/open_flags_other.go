//go:build !darwin && !linux

package main

// Platforms without the two known-safe flags retain the pre-open and
// post-open identity checks in openPrivateDestination.
const secureOpenFlags = 0
