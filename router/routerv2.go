// Copyright 2021 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package router provides interfaces that need to be satisfied in order to
// implement a new router on tsuru.

package router

import (
	"context"
)

type BackendPrefix struct {
	Prefix string
	Target map[string]string // in kubernetes cluster be like {serviceName: "", namespace: ""}
}

type EnsureBackendOpts struct {
	Opts     map[string]interface{}
	Prefixes []BackendPrefix
}

// RouterV2 is specialized in clustered router environments like kubernetes
// after deprecation of previous router, we could just use the interface bellow
type RouterV2 interface {
	EnsureBackend(ctx context.Context, app App, o EnsureBackendOpts) error
}
