package main

import "sync/atomic"

type daemonRuntime struct {
	statsPath atomic.Value
}

func newDaemonRuntime(statsPath string) *daemonRuntime {
	runtime := &daemonRuntime{}
	runtime.SetStatsPath(statsPath)
	return runtime
}

func (r *daemonRuntime) StatsPath() string {
	if r == nil {
		return ""
	}
	if value, ok := r.statsPath.Load().(string); ok {
		return value
	}
	return ""
}

func (r *daemonRuntime) SetStatsPath(statsPath string) {
	if r == nil {
		return
	}
	r.statsPath.Store(statsPath)
}
