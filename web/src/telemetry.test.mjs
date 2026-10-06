import assert from "node:assert/strict";
import { test } from "node:test";
import {
  filterRequests,
  formatDuration,
  keyLabel,
  resultLabel,
  unknownUsage,
} from "./telemetry.ts";

const record = (fields = {}) => ({
  id: "r1",
  keyId: "k1",
  keyName: "CI",
  status: 200,
  usageKnown: true,
  outcome: "success",
  ...fields,
});

test("formatDuration picks a unit that fits", () => {
  assert.equal(formatDuration(0), "0 ms");
  assert.equal(formatDuration(840.4), "840 ms");
  assert.equal(formatDuration(3240), "3.2 s");
  assert.equal(formatDuration(125000), "2m 05s");
  assert.equal(formatDuration(-1), "");
  assert.equal(formatDuration(NaN), "");
});

test("resultLabel names errors and cut-short responses, with or without a status", () => {
  assert.equal(resultLabel(record()), "200");
  assert.equal(
    resultLabel(record({ outcome: "error", status: 502 })),
    "502 error",
  );
  assert.equal(resultLabel(record({ outcome: "error", status: 0 })), "Failed");
  assert.equal(
    resultLabel(record({ outcome: "incomplete", status: 200 })),
    "200, cut short",
  );
  assert.equal(
    resultLabel(record({ outcome: "incomplete", status: 0 })),
    "Cut short",
  );
});

test("keyLabel survives deleted and server keys", () => {
  assert.equal(keyLabel(record()), "CI");
  assert.equal(keyLabel(record({ keyName: "" })), "Unnamed key");
  assert.equal(keyLabel(record({ keyName: "", keyId: "" })), "Server key");
});

test("filterRequests combines the result and key filters", () => {
  const rows = [
    record({ id: "a" }),
    record({ id: "b", outcome: "error", keyName: "Laptop" }),
    record({ id: "c", outcome: "incomplete" }),
    record({ id: "d", outcome: "error" }),
  ];
  const ids = (list) => list.map((r) => r.id).join("");
  assert.equal(ids(filterRequests(rows, "all", "")), "abcd");
  assert.equal(ids(filterRequests(rows, "error", "")), "bd");
  assert.equal(ids(filterRequests(rows, "error", "CI")), "d");
  assert.equal(ids(filterRequests(rows, "incomplete", "Laptop")), "");
});

test("unknownUsage counts rows without a usage report, not zero-token rows", () => {
  const rows = [
    record({ totalTokens: 0 }),
    record({ usageKnown: false }),
    record({ usageKnown: false, outcome: "incomplete" }),
  ];
  assert.equal(unknownUsage(rows), 2);
});
