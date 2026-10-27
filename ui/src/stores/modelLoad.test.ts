import { afterEach, describe, expect, it, vi } from "vitest";
import { loadingMessage, loadingPercent, onToggleLoad } from "./modelLoad";
import type { Model, ModelStatus } from "../lib/types";

function model(fields: Partial<Model>): Model {
  return { id: "m", state: "stopped", name: "", description: "", unlisted: false, peerID: "", ...fields };
}

describe("loadingPercent", () => {
  it("rounds the reported fraction down to a whole percentage", () => {
    expect(loadingPercent(model({ state: "starting", loadingProgress: 0.426 }))).toBe(42);
    expect(loadingPercent(model({ state: "starting", loadingProgress: 0 }))).toBe(0);
    // kyojin caps progress at 0.999 until it is ready
    expect(loadingPercent(model({ state: "starting", loadingProgress: 0.999 }))).toBe(99);
  });

  it("is undefined without progress or once the model is no longer starting", () => {
    expect(loadingPercent(model({ state: "starting" }))).toBeUndefined();
    expect(loadingPercent(model({ state: "ready", loadingProgress: 1 }))).toBeUndefined();
    expect(loadingPercent(undefined)).toBeUndefined();
  });
});

describe("loadingMessage", () => {
  it("is only shown while starting", () => {
    expect(loadingMessage(model({ state: "starting", loadingMessage: "loading tensors" }))).toBe("loading tensors");
    expect(loadingMessage(model({ state: "ready", loadingMessage: "loading tensors" }))).toBe("");
    expect(loadingMessage(model({ state: "starting" }))).toBe("");
  });
});

describe("onToggleLoad", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it.each(["ready", "starting", "sleeping", "waking", "going-to-sleep"] as ModelStatus[])(
    "unloads a %s model",
    (state) => {
      const fetchMock = vi.fn().mockResolvedValue(new Response("OK"));
      vi.stubGlobal("fetch", fetchMock);

      onToggleLoad(model({ state }));

      expect(fetchMock).toHaveBeenCalledWith("/api/models/unload/m", { method: "POST" });
    }
  );

  it("does nothing while a model is stopping", () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    onToggleLoad(model({ state: "stopping" }));

    expect(fetchMock).not.toHaveBeenCalled();
  });
});
