import { get } from "svelte/store";
import { describe, expect, it } from "vitest";
import {
  handleAPIEventMessage,
  inFlightRequests,
  inflightRequestEntries,
  metrics,
} from "./api";
import type { ActivityLogEntry } from "../lib/types";

describe("api store event handling", () => {
  it("parses inflight request entries", () => {
    inFlightRequests.set(0);
    inflightRequestEntries.set([]);

    handleAPIEventMessage(
      JSON.stringify({
        type: "inflight",
        data: JSON.stringify({
          requests: [
            {
              id: "7",
              timestamp: "2026-07-03T00:00:00Z",
              model: "m1",
              req_path: "/v1/chat/completions",
              method: "POST",
              metadata: { source: "test" },
            },
          ],
        }),
      })
    );

    expect(get(inFlightRequests)).toBe(1);
    expect(get(inflightRequestEntries)).toEqual([
      {
        id: "7",
        timestamp: "2026-07-03T00:00:00Z",
        model: "m1",
        req_path: "/v1/chat/completions",
        method: "POST",
        metadata: { source: "test" },
      },
    ]);
  });

  it("caps the metrics store at 100 entries, keeping the newest", () => {
    metrics.set([]);

    const makeEntry = (id: number): ActivityLogEntry => ({
      id,
      timestamp: "2026-07-03T00:00:00Z",
      model: "m1",
      req_path: "/v1/chat/completions",
      resp_content_type: "application/json",
      resp_status_code: 200,
      tokens: {
        cache_tokens: -1,
        draft_tokens: -1,
        draft_acc_tokens: -1,
        input_tokens: 1,
        output_tokens: 1,
        prompt_per_second: -1,
        tokens_per_second: -1,
      },
      duration_ms: 1,
      has_capture: false,
    });

    for (let id = 1; id <= 120; id++) {
      handleAPIEventMessage(
        JSON.stringify({ type: "metrics", data: JSON.stringify([makeEntry(id)]) })
      );
    }

    const stored = get(metrics);
    expect(stored).toHaveLength(100);
    expect(stored[0].id).toBe(120);
    expect(stored[99].id).toBe(21);
  });
});
