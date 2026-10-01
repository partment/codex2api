import assert from "node:assert/strict";
import test from "node:test";

import {
  DEFAULT_AUTO_FAST_MIN_REMAINING_RATIO,
  normalizeAutoFastMinRemainingRatio,
} from "./autoFastSettings.ts";
import { buildWritableSettingsPayload } from "./settingsPayload.ts";

test("Auto Fast preserves zero, boundaries, and valid configured ratios", () => {
  for (const ratio of [0, 0.2, 0.5, 0.8, 1]) {
    const normalized = normalizeAutoFastMinRemainingRatio(ratio);
    assert.equal(normalized, ratio);
    const payload = buildWritableSettingsPayload({
      codex_priority_service_tier_enabled: true,
      codex_priority_service_tier_min_remaining_ratio: normalized,
    });
    assert.equal(payload.codex_priority_service_tier_enabled, true);
    assert.equal(payload.codex_priority_service_tier_min_remaining_ratio, ratio);
  }
});

test("Auto Fast defaults only missing, invalid, or non-finite ratios", () => {
  assert.equal(DEFAULT_AUTO_FAST_MIN_REMAINING_RATIO, 0.5);
  for (const ratio of [undefined, null, -0.1, 1.1, NaN, Infinity, -Infinity, "0"]) {
    assert.equal(normalizeAutoFastMinRemainingRatio(ratio), 0.5);
  }
});

test("Auto Fast toggle can be disabled without losing the zero threshold", () => {
  assert.deepEqual(buildWritableSettingsPayload({
    codex_priority_service_tier_enabled: false,
    codex_priority_service_tier_min_remaining_ratio: 0,
  }), {
    codex_priority_service_tier_enabled: false,
    codex_priority_service_tier_min_remaining_ratio: 0,
  });
});
