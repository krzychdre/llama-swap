# vLLM Sleep/Wake Support for llama-swap

## Context

vLLM models take 2+ minutes to cold-start. vLLM provides sleep/wake API endpoints that free/restore VRAM while keeping the process alive. Instead of killing vLLM processes during model swaps (forcing a full restart), llama-swap should put them to sleep and wake them on demand — reducing swap time from minutes to seconds.

vLLM exposes these endpoints on the model server:
- `POST /sleep` — free VRAM, keep process alive
- `POST /wake_up` — reload model into VRAM
- `GET /is_sleeping` — query current sleep state

---

## 1. Config — `proxy/config/model_config.go`

Add `SleepWakeConfig` struct and field to `ModelConfig`:

```go
type SleepWakeConfig struct {
    Enabled            bool   `yaml:"enabled"`
    SleepEndpoint      string `yaml:"sleepEndpoint"`      // default: "/sleep"
    WakeEndpoint       string `yaml:"wakeEndpoint"`        // default: "/wake_up"
    IsSleepingEndpoint string `yaml:"isSleepingEndpoint"`  // default: "/is_sleeping"
}

type ModelConfig struct {
    // ... existing fields ...
    SleepWake SleepWakeConfig `yaml:"sleepWake"`
}
```

Defaults in `UnmarshalYAML`: `SleepEndpoint: "/sleep"`, `WakeEndpoint: "/wake_up"`, `IsSleepingEndpoint: "/is_sleeping"`, `Enabled: false`.

YAML example:
```yaml
models:
  Qwen3.6-27B:
    cmd: "vllm serve Qwen/Qwen3-27B ..."
    proxy: "http://localhost:${PORT}"
    sleepWake:
      enabled: true
```

### Files to modify:
- [proxy/config/model_config.go](proxy/config/model_config.go) — add `SleepWakeConfig` struct, `SleepWake` field, defaults
- [config-schema.json](config-schema.json) — add `sleepWake` property to model schema
- [config.example.yaml](config.example.yaml) — add sleep/wake example

---

## 2. Process States — `proxy/process.go`

Add two new states:

```go
StateSleeping ProcessState = "sleeping"  // process alive, VRAM freed
StateWaking   ProcessState = "waking"    // process alive, loading VRAM
```

### State transitions

```
stopped  -> starting  (normal cold start)
starting -> ready     (health check passes)
ready    -> sleeping  (sleep endpoint called, instead of stop)
sleeping -> waking    (wake endpoint called, on new request)
waking   -> ready     (health check passes after wake)
ready    -> stopping  (explicit unload / non-sleep eviction)
sleeping -> stopping  (explicit unload while sleeping)
waking   -> stopping  (wake fails or explicit unload)
```

Update `isValidTransition()` in `proxy/process.go:206-220`:
- `StateReady -> StateSleeping` (new)
- `StateSleeping -> StateWaking` (new)
- `StateSleeping -> StateStopping` (new)
- `StateWaking -> StateReady` (new)
- `StateWaking -> StateStopping` (new)

### Files to modify:
- [proxy/process.go](proxy/process.go) — add constants, update `isValidTransition()`

---

## 3. Process Methods — `proxy/process.go`

### `Sleep()` method

Wait for in-flight requests, send `POST` to sleep endpoint, transition to `StateSleeping`. On error, fall back to normal `Stop()`.

### `Wake()` method

Transition to `StateWaking`, send `POST` to wake endpoint, then run health check loop (reuse existing `checkHealthEndpoint` logic). On success: `StateWaking -> StateReady`. On failure: `StateWaking -> StateStopping` and `stopCommand()`.

### `IsSleeping()` method

Send `GET` to is_sleeping endpoint to query the vLLM process sleep state. Used to detect if a "ready" process has been externally put to sleep, or to verify sleep completed.

### Modify `start()`

In `start()` (line ~294), before the normal process launch logic, check for sleeping state:

```go
func (p *Process) start() error {
    if p.CurrentState() == StateSleeping {
        return p.wake()  // fast path: wake existing process
    }
    // ... existing start logic ...
}
```

### Modify `waitForCmd()`

The existing default case in `waitForCmd()` (line 678) already forces `StateStopped` for unexpected exits. This handles process crashes while sleeping — no change needed.

### Files to modify:
- [proxy/process.go](proxy/process.go) — add `Sleep()`, `Wake()`, `IsSleeping()` methods; modify `start()`

---

## 4. Swap Logic — ProcessGroup and Matrix

### ProcessGroup — `proxy/processgroup.go:92`

Change the swap-away stop call:

```go
// line 92: currently
pg.processes[pg.lastUsedProcess].Stop()

// change to:
process := pg.processes[pg.lastUsedProcess]
if process.IsSleepWakeEnabled() {
    process.Sleep()
} else {
    process.Stop()
}
```

### Matrix — `proxy/matrix.go:222-231`

Change the eviction loop:

```go
for _, evictModel := range result.Evict {
    if p, exists := m.processes[evictModel]; exists {
        wg.Add(1)
        go func(p *Process) {
            defer wg.Done()
            if p.IsSleepWakeEnabled() {
                p.Sleep()
            } else {
                p.Stop()
            }
        }(p)
    }
}
```

### `StopProcesses` methods

The `StopProcesses()` in both ProcessGroup and Matrix do full stops — this is correct for explicit unload-all. No change needed there; sleep-enabled models should fully stop on explicit unload.

### TTL behavior

When a sleep-enabled model's TTL expires, it should sleep (not stop) to preserve the fast wake capability. Modify the TTL goroutine in `process.go:400-423`:

```go
if p.config.SleepWake.Enabled {
    p.Sleep()  // instead of p.Stop()
} else {
    p.Stop()
}
```

### Files to modify:
- [proxy/processgroup.go](proxy/processgroup.go) — line 92, swap-away logic
- [proxy/matrix.go](proxy/matrix.go) — lines 222-231, eviction logic
- [proxy/process.go](proxy/process.go) — TTL goroutine (~line 420)

---

## 5. API Endpoints — `proxy/proxymanager_api.go`

### New API routes (RESTful, following `{model}` path param pattern)

```
POST /api/models/{model}/sleep   — manually put model to sleep
POST /api/models/{model}/wake    — manually wake a sleeping model
```

### Update `Model` struct

Add `SleepWakeEnabled bool` field so the UI knows which models support sleep/wake:

```go
type Model struct {
    // ... existing fields ...
    SleepWakeEnabled bool `json:"sleepWakeEnabled,omitempty"`
}
```

### Update SSE model status

Add `StateSleeping` and `StateWaking` cases to the state switch in `getModels()` (line ~69).

### Files to modify:
- [proxy/proxymanager_api.go](proxy/proxymanager_api.go) — new handlers, Model struct, state switch
- [proxy/proxymanager.go](proxy/proxymanager.go) — route registration in `addApiHandlers`

---

## 6. UI Changes — `ui-svelte/`

### Types — `ui-svelte/src/lib/types.ts`

```typescript
export type ModelStatus = "ready" | "starting" | "stopping" | "stopped" | "shutdown" | "sleeping" | "waking" | "unknown";

export interface Model {
  // ... existing fields ...
  sleepWakeEnabled?: boolean;
}
```

### API Store — `ui-svelte/src/stores/api.ts`

Add `sleepModel()` and `wakeModel()` functions:
```typescript
export async function sleepModel(model: string): Promise<void> {
  await fetch(`/api/models/${model}/sleep`, { method: "POST" });
}

export async function wakeModel(model: string): Promise<void> {
  await fetch(`/api/models/${model}/wake`, { method: "POST" });
}
```

### ModelsPanel — `ui-svelte/src/components/ModelsPanel.svelte`

Update the action button column (line 172-177) to show:
- **Stopped**: "Load" button (existing)
- **Ready + sleep-enabled**: "Sleep" button + "Unload" button
- **Sleeping**: "Wake" button
- **Waking**: spinner/disabled button
- **Ready + no sleep**: "Unload" button (existing)

### Status styles — `ui-svelte/src/index.css`

```css
.status--sleeping {
  @apply bg-purple-500/10 text-purple-500;
}
.status--waking {
  @apply bg-warning/10 text-warning;
}
```

### Files to modify:
- [ui-svelte/src/lib/types.ts](ui-svelte/src/lib/types.ts) — update `ModelStatus`, `Model`
- [ui-svelte/src/stores/api.ts](ui-svelte/src/stores/api.ts) — add `sleepModel()`, `wakeModel()`
- [ui-svelte/src/components/ModelsPanel.svelte](ui-svelte/src/components/ModelsPanel.svelte) — update buttons and imports
- [ui-svelte/src/index.css](ui-svelte/src/index.css) — add status styles

---

## 7. Testing

### Go tests
- `TestProcess_SleepWake` — test sleep/wake state transitions, endpoint calls
- `TestProcess_SleepWakeOnSwap` — test ProcessGroup swap uses Sleep() for enabled models
- `TestProcessGroup_SleepWakeSwap` — integration test: swap between sleep-enabled and normal models
- `TestMatrix_SleepWakeEviction` — test matrix eviction uses Sleep() for enabled models
- `TestProcess_WakeFromSleep` — test that `start()` detects sleeping state and wakes
- `TestProcess_SleepFailsFallsBackToStop` — test fallback when sleep endpoint fails
- `TestProcess_WakeTimeout` — test wake timeout falls back to full stop

Run with: `go test -v -run TestProcess_SleepWake`
Then: `make test-dev` for full suite.

### UI tests
Run `make test-ui` after UI changes.

---

## Implementation Order

1. Config (model_config.go, config-schema.json, example)
2. Process states (process.go — constants, transitions)
3. Process methods (process.go — Sleep, Wake, IsSleeping, start modification)
4. Swap logic (processgroup.go, matrix.go)
5. API endpoints (proxymanager_api.go, proxymanager.go)
6. UI (types.ts, api.ts, ModelsPanel.svelte, index.css)
7. Tests
8. `make test-dev` + `make test-ui` + `make test-all`
