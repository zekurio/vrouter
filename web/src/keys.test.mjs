import assert from "node:assert/strict";
import { test } from "node:test";
import { keyState, limitText, parseLimit, usedShare } from "./keys.ts";

const key = (fields = {}) => ({
  id: "k1",
  name: "CI",
  prefix: "vr_ab12",
  createdAt: "2026-10-01T00:00:00Z",
  limitRequests: 0,
  limitTokens: 0,
  usedRequests: 0,
  usedTokens: 0,
  ...fields,
});

test("a key with no limits stays active however much it is used", () => {
  assert.equal(keyState(key({ usedRequests: 9e6, usedTokens: 9e9 })), "active");
});

test("a limit is spent once usage reaches it, including past it", () => {
  assert.equal(keyState(key({ limitRequests: 10, usedRequests: 9 })), "active");
  assert.equal(
    keyState(key({ limitRequests: 10, usedRequests: 10 })),
    "requests-spent",
  );
  assert.equal(
    keyState(key({ limitTokens: 1000, usedTokens: 1840 })),
    "tokens-spent",
  );
});

test("revoked and unknown-usage blocks outrank spent limits", () => {
  const spent = { limitTokens: 10, usedTokens: 10 };
  assert.equal(keyState(key({ ...spent, usageUncertain: true })), "uncertain");
  assert.equal(
    keyState(
      key({
        ...spent,
        usageUncertain: true,
        revokedAt: "2026-10-02T00:00:00Z",
      }),
    ),
    "revoked",
  );
  assert.equal(
    keyState(key({ ...spent, usageUncertain: false })),
    "tokens-spent",
  );
});

test("usedShare caps an overshot token limit at a full bar", () => {
  assert.equal(usedShare(250, 1000), 25);
  assert.equal(usedShare(1840, 1000), 100);
  assert.equal(usedShare(50, 0), 0);
});

test("parseLimit reads blank as no limit and rejects anything but whole numbers", () => {
  assert.deepEqual(parseLimit(""), { value: 0 });
  assert.deepEqual(parseLimit("  "), { value: 0 });
  assert.deepEqual(parseLimit("0"), { value: 0 });
  assert.deepEqual(parseLimit("1,000,000"), { value: 1000000 });
  assert.deepEqual(parseLimit(" 2 500 "), { value: 2500 });
  for (const bad of ["-5", "1.5", "1e6", "ten", "5k"])
    assert.ok("error" in parseLimit(bad), bad);
  assert.ok("error" in parseLimit("99999999999999999999"));
});

test("limitText round-trips through parseLimit", () => {
  assert.equal(limitText(0), "");
  assert.deepEqual(parseLimit(limitText(2500)), { value: 2500 });
});
