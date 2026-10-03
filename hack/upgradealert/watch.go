package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

const hookWatchTimeout = 30 * time.Second

func (o *observer) watch(ctx context.Context) error {
	for ctx.Err() == nil {
		s := o.snapshot()
		if s.HistoryLost {
			<-ctx.Done()
			return nil
		}
		seconds := int64(hookWatchTimeout / time.Second)
		stream, err := o.jobs.BatchV1().Jobs(s.Intent.Namespace).Watch(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + s.Intent.HookJob, ResourceVersion: s.ResourceVersion, AllowWatchBookmarks: true, TimeoutSeconds: &seconds})
		if err != nil {
			if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
				if err := o.loseHistory(); err != nil {
					return err
				}
				continue
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
				continue
			}
		}
		o.setWatching(true)
		err = o.segment(ctx, stream)
		stream.Stop()
		o.setWatching(false)
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
	return nil
}

func (o *observer) loseHistory() error {
	s := o.snapshot()
	s.HistoryLost = true
	// A history gap after a verified recovery invalidates continued monitoring,
	// but does not rewrite that historical recovery. No further transaction may
	// be started with this state file.
	if s.RecoveredAt != nil {
		return errors.New("hook history ended after verified recovery")
	}
	return o.persist(s)
}

func (o *observer) segment(ctx context.Context, stream watch.Interface) error {
	recovery := time.NewTicker(2 * time.Second)
	defer recovery.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-stream.ResultChan():
			if !ok {
				return nil
			} // Resume the last durable cursor; never relist over a gap.
			if event.Type == watch.Error {
				err := apierrors.FromObject(event.Object)
				if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
					return o.loseHistory()
				}
				return nil
			}
			object, err := meta.Accessor(event.Object)
			if err != nil || object.GetResourceVersion() == "" {
				return o.loseHistory()
			}
			s := o.snapshot()
			switch event.Type {
			case watch.Bookmark:
			case watch.Added, watch.Modified, watch.Deleted:
				job, ok := event.Object.(*batchv1.Job)
				if !ok {
					return o.loseHistory()
				}
				if err := s.observe(job, time.Now().UTC()); err != nil {
					return fmt.Errorf("refuse unbound upgrade hook: %w", err)
				}
			default:
				return o.loseHistory()
			}
			s.ResourceVersion = object.GetResourceVersion()
			if err := o.persist(s); err != nil {
				return err
			}
		case <-recovery.C:
			s := o.snapshot()
			if s.RecoveredAt != nil || s.HistoryLost || len(s.Attempts) == 0 {
				continue
			}
			a := s.Attempts[len(s.Attempts)-1]
			if a.CompletedAt == nil {
				continue
			}
			checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := verifyRecovery(checkCtx, o.resources, s)
			cancel()
			if err != nil {
				continue
			}
			// A fresh Job read prevents clearing over a retry already visible to the
			// API but not yet delivered to this watch.
			readCtx, stopRead := context.WithTimeout(ctx, 10*time.Second)
			current, err := o.jobs.BatchV1().Jobs(s.Intent.Namespace).Get(readCtx, s.Intent.HookJob, metav1.GetOptions{})
			stopRead()
			if err != nil && !apierrors.IsNotFound(err) {
				continue
			}
			if err == nil && (string(current.UID) != a.UID || s.observe(current, time.Now().UTC()) != nil) {
				continue
			}
			now := time.Now().UTC()
			s.RecoveredAt = &now
			s.RecoveryJobUID = a.UID
			if err := o.persist(s); err != nil {
				return err
			}
		}
	}
}
