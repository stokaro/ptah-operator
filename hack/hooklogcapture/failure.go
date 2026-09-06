package main

import (
	"context"
	"errors"
	"fmt"
)

type failureClass string

const (
	failureClassConfiguration     failureClass = "configuration"
	failureClassOutput            failureClass = "output"
	failureClassRender            failureClass = "render"
	failureClassKubernetesClient  failureClass = "kubernetes-client"
	failureClassPriorityInventory failureClass = "priority-inventory"
	failureClassPriorityWatch     failureClass = "priority-watch"
	failureClassJobInventory      failureClass = "job-inventory"
	failureClassJobWatch          failureClass = "job-watch"
	failureClassJobContract       failureClass = "job-contract"
	failureClassPodInventory      failureClass = "pod-inventory"
	failureClassPodWatch          failureClass = "pod-watch"
	failureClassPodContract       failureClass = "pod-contract"
	failureClassPodOwner          failureClass = "pod-owner"
	failureClassLogStart          failureClass = "log-start"
	failureClassLogStartTimeout   failureClass = "log-start-timeout"
	failureClassLogRead           failureClass = "log-read"
	failureClassLogEmpty          failureClass = "log-empty"
	failureClassLogTooLarge       failureClass = "log-too-large"
	failureClassDeadline          failureClass = "deadline"
	failureClassCanceled          failureClass = "canceled"
	failureClassInternal          failureClass = "internal"
)

var validFailureClasses = map[failureClass]struct{}{
	failureClassConfiguration:     {},
	failureClassOutput:            {},
	failureClassRender:            {},
	failureClassKubernetesClient:  {},
	failureClassPriorityInventory: {},
	failureClassPriorityWatch:     {},
	failureClassJobInventory:      {},
	failureClassJobWatch:          {},
	failureClassJobContract:       {},
	failureClassPodInventory:      {},
	failureClassPodWatch:          {},
	failureClassPodContract:       {},
	failureClassPodOwner:          {},
	failureClassLogStart:          {},
	failureClassLogStartTimeout:   {},
	failureClassLogRead:           {},
	failureClassLogEmpty:          {},
	failureClassLogTooLarge:       {},
	failureClassDeadline:          {},
	failureClassCanceled:          {},
	failureClassInternal:          {},
}

type classifiedFailure struct {
	class failureClass
	cause error
}

func (failure *classifiedFailure) Error() string {
	return failure.cause.Error()
}

func (failure *classifiedFailure) Unwrap() error {
	return failure.cause
}

func classifyFailure(class failureClass, cause error) error {
	if cause == nil {
		return nil
	}
	var existing *classifiedFailure
	if errors.As(cause, &existing) {
		return cause
	}
	if _, found := validFailureClasses[class]; !found {
		return &classifiedFailure{class: failureClassInternal, cause: cause}
	}
	return &classifiedFailure{class: class, cause: cause}
}

func classifiedFailuref(class failureClass, format string, arguments ...any) error {
	return classifyFailure(class, fmt.Errorf(format, arguments...))
}

func classifyContextFailure(cause error) error {
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		return classifyFailure(failureClassDeadline, cause)
	case errors.Is(cause, context.Canceled):
		return classifyFailure(failureClassCanceled, cause)
	default:
		return classifyFailure(failureClassInternal, cause)
	}
}

func failureClassFor(cause error) failureClass {
	var failure *classifiedFailure
	if errors.As(cause, &failure) {
		if _, found := validFailureClasses[failure.class]; found {
			return failure.class
		}
	}
	return failureClassInternal
}
