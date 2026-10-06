import assert from "node:assert/strict";
import { test } from "node:test";
import { isDialogBackdropClick } from "./dialog.ts";

const dialog = {
  getBoundingClientRect: () => ({ left: 35, right: 595, top: 25, bottom: 605 }),
};
const click = (clientX, clientY, target = dialog) =>
  isDialogBackdropClick({ currentTarget: dialog, target, clientX, clientY });

test("dialog padding, empty space and border clicks stay open", () => {
  for (const [x, y] of [
    [315, 30],
    [40, 315],
    [590, 315],
    [315, 600],
    [315, 315],
    [35, 25],
    [595, 605],
  ])
    assert.equal(click(x, y), false, `inside at ${x}, ${y}`);
});

test("backdrop clicks outside each edge dismiss", () => {
  for (const [x, y] of [
    [315, 24],
    [34, 315],
    [596, 315],
    [315, 606],
  ])
    assert.equal(click(x, y), true, `outside at ${x}, ${y}`);
});

test("child clicks and keyboard clicks do not count as backdrop clicks", () => {
  assert.equal(click(315, 68, {}), false);
  assert.equal(click(0, 0, {}), false);
});
