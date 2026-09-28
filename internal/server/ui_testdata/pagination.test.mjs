import assert from "node:assert/strict";
import test from "node:test";

import {cursorPageHistory, normalizePage, pageWindow, singleFlight} from "../ui/pagination.js";

test("paged resource responses preserve their continuation cursor", () => {
  const page = normalizePage({items: [{name: "fast"}], nextCursor: "next-page"});
  assert.deepEqual(page.items, [{name: "fast"}]);
  assert.equal(page.nextCursor, "next-page");
});

test("numbered page window keeps first, last, current, and neighbours with gaps", () => {
  // Few pages: every page is shown, no ellipsis.
  assert.deepEqual(pageWindow(1, 3), [1, 2, 3]);
  // Middle of a long range: first, last, current +/- 1, ellipsis for the gaps.
  assert.deepEqual(pageWindow(5, 10), [1, null, 4, 5, 6, null, 10]);
  // Near the start: no leading ellipsis, trailing gap collapsed.
  assert.deepEqual(pageWindow(2, 10), [1, 2, 3, null, 10]);
  // Single page collapses to one entry.
  assert.deepEqual(pageWindow(1, 1), [1]);
});

test("rapid load-more requests cannot append the same cursor twice", async () => {
  let release;
  const response = new Promise((resolve) => { release = resolve; });
  const requestedCursors = [];
  const appended = [];
  const busyStates = [];
  let cursor = "page-2";
  const loadMore = singleFlight(async () => {
    requestedCursors.push(cursor);
    const page = await response;
    appended.push(...page.items);
    cursor = page.nextCursor;
  }, (busy) => busyStates.push(busy));

  const first = loadMore();
  const duplicate = loadMore();
  assert.equal(await duplicate, false);
  release({items: ["asset-51"], nextCursor: "page-3"});
  assert.equal(await first, true);

  assert.deepEqual(requestedCursors, ["page-2"]);
  assert.deepEqual(appended, ["asset-51"]);
  assert.equal(cursor, "page-3");
  assert.deepEqual(busyStates, [true, false]);
});

test("asset cursor history supports previous and next and resets on a new filter", () => {
  const pages = cursorPageHistory();
  assert.equal(pages.cursor(0), "");
  pages.record(0, "cursor-two");
  assert.equal(pages.cursor(1), "cursor-two");
  pages.record(1, "cursor-three");
  assert.equal(pages.cursor(2), "cursor-three");
  assert.equal(pages.cursor(0), "");
  pages.record(0, "replacement-cursor");
  assert.equal(pages.cursor(1), "replacement-cursor");
  assert.throws(() => pages.cursor(2), RangeError);
  pages.reset();
  assert.equal(pages.cursor(0), "");
  assert.throws(() => pages.cursor(1), RangeError);
});
