package orm

import (
	"context"
	"reflect"
)

// Lifecycle hooks. Implement any of these on the model's pointer receiver.
// Returning an error from a Before hook aborts the operation; inside a
// Transaction it also rolls the transaction back.
type (
	BeforeInserter interface {
		BeforeInsert(ctx context.Context) error
	}
	AfterInserter interface {
		AfterInsert(ctx context.Context) error
	}
	BeforeUpdater interface {
		BeforeUpdate(ctx context.Context) error
	}
	AfterUpdater interface {
		AfterUpdate(ctx context.Context) error
	}
	BeforeDeleter interface {
		BeforeDelete(ctx context.Context) error
	}
	AfterDeleter interface {
		AfterDelete(ctx context.Context) error
	}
	AfterFinder interface {
		AfterFind(ctx context.Context) error
	}
)

type hookKind int

const (
	hookBeforeInsert hookKind = iota
	hookAfterInsert
	hookBeforeUpdate
	hookAfterUpdate
	hookBeforeDelete
	hookAfterDelete
	hookAfterFind
)

// callHook invokes the hook on an addressable struct value, if implemented.
func callHook(ctx context.Context, v reflect.Value, k hookKind) error {
	if !v.CanAddr() {
		return nil
	}
	x := v.Addr().Interface()
	switch k {
	case hookBeforeInsert:
		if h, ok := x.(BeforeInserter); ok {
			return h.BeforeInsert(ctx)
		}
	case hookAfterInsert:
		if h, ok := x.(AfterInserter); ok {
			return h.AfterInsert(ctx)
		}
	case hookBeforeUpdate:
		if h, ok := x.(BeforeUpdater); ok {
			return h.BeforeUpdate(ctx)
		}
	case hookAfterUpdate:
		if h, ok := x.(AfterUpdater); ok {
			return h.AfterUpdate(ctx)
		}
	case hookBeforeDelete:
		if h, ok := x.(BeforeDeleter); ok {
			return h.BeforeDelete(ctx)
		}
	case hookAfterDelete:
		if h, ok := x.(AfterDeleter); ok {
			return h.AfterDelete(ctx)
		}
	case hookAfterFind:
		if h, ok := x.(AfterFinder); ok {
			return h.AfterFind(ctx)
		}
	}
	return nil
}
