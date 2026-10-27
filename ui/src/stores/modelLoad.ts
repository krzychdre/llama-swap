import { writable } from "svelte/store";
import { loadModel, unloadSingleModel } from "./api";
import type { Model, ModelStatus } from "../lib/types";

export const pendingLoads = writable<Record<string, boolean>>({});
const loadControllers = new Map<string, AbortController>();

export async function handleLoadModel(id: string): Promise<void> {
  if (isPending(id)) return;

  const controller = new AbortController();
  loadControllers.set(id, controller);
  pendingLoads.update((p) => ({ ...p, [id]: true }));
  try {
    await loadModel(id, controller.signal);
  } catch (e) {
    console.error(e);
  } finally {
    loadControllers.delete(id);
    pendingLoads.update((p) => {
      const next = { ...p };
      delete next[id];
      return next;
    });
  }
}

export function cancelLoad(id: string): void {
  loadControllers.get(id)?.abort();
}

export function isPending(id: string): boolean {
  let val = false;
  pendingLoads.subscribe((p) => (val = !!p[id]))();
  return val;
}

// States an unload request can stop, including a load or sleep/wake still in progress.
const stoppableStates: ReadonlySet<ModelStatus> = new Set<ModelStatus>([
  "ready",
  "starting",
  "sleeping",
  "waking",
  "going-to-sleep",
]);

export function canStop(m: Model): boolean {
  return stoppableStates.has(m.state);
}

export function onToggleLoad(m: Model): void {
  if (m.state === "stopped" && isPending(m.id)) {
    cancelLoad(m.id);
  } else if (m.state === "stopped") {
    void handleLoadModel(m.id);
  } else if (canStop(m)) {
    void unloadSingleModel(m.id);
  }
}

// loadingPercent is the reported loading progress as a whole percentage, or
// undefined when the model is not starting or its upstream reports none. It
// rounds down so a load that is not finished never shows 100%.
export function loadingPercent(m: Model | undefined): number | undefined {
  if (m?.state !== "starting" || m.loadingProgress === undefined) return undefined;
  return Math.floor(Math.min(Math.max(m.loadingProgress, 0), 1) * 100);
}

// loadingMessage is the upstream's reported loading step while starting.
export function loadingMessage(m: Model | undefined): string {
  return m?.state === "starting" ? (m.loadingMessage ?? "") : "";
}

export function statusDotColor(m: Model | undefined): string {
  if (!m) return "bg-muted-foreground/40";
  if (m.state === "ready") return "bg-success";
  if (m.state === "starting" || m.state === "stopping") return "bg-warning";
  if (m.state === "sleeping") return "bg-blue-500";
  if (m.state === "waking" || m.state === "going-to-sleep") return "bg-warning";
  return "bg-muted-foreground/40";
}
