import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import test from "node:test";
import vm from "node:vm";

// Exercise the form's submit handler, including whether a request is sent.
function uploadHarness() {
  const requests = [];
  const notices = [];
  const nodes = [];
  const scope = {
    element(tag) {
      const node = {tag, children: [], listeners: {}, value: "", resets: 0,
        append(...children) { this.children.push(...children); },
        addEventListener(event, callback) { this.listeners[event] = callback; },
        reset() { this.resets += 1; },
      };
      nodes.push(node);
      return node;
    },
    api: {upload: async (...args) => requests.push(args)},
    showNotice: (message) => notices.push(message),
  };
  const source = readFileSync(new URL("../ui/repositories.js", import.meta.url), "utf8")
    .replace(/import\s[\s\S]*?from\s+"[^"]+";/g, "")
    .replace(/export /g, "");
  vm.runInNewContext(source, scope);
  scope.uploadPanel("releases");
  const form = nodes.find((node) => node.tag === "form");
  const [path, file] = nodes.filter((node) => node.tag === "input");
  file.files = [{name: "artifact"}];
  return {requests, notices, form, async submit(value) {
    path.value = value;
    await form.listeners.submit({preventDefault() {}});
  }};
}

test("Raw upload rejects paths browsers would normalize before any request", async () => {
  for (const path of ["a/../b", "", ".", "..", "a/./b", "/a", "a/", "a//b", "a\\b", "a\nb", "a\x00b", "a\x7fb"]) {
    const harness = uploadHarness();
    await harness.submit(path);
    assert.equal(harness.requests.length, 0, JSON.stringify(path));
    assert.match(harness.notices[0], /path/i);
    assert.equal(harness.form.resets, 0);
  }
});

test("Raw upload preserves literal percent escapes, punctuation, and Unicode", async () => {
  const harness = uploadHarness();
  await harness.submit("release notes/%2e%2e/100% café?#.txt");
  assert.equal(harness.requests.length, 1);
  const url = new URL(harness.requests[0][0], "https://example.test");
  assert.equal(url.pathname, "/repository/releases/release%20notes/%252e%252e/100%25%20caf%C3%A9%3F%23.txt");
  assert.equal(url.search, "");
  assert.equal(url.hash, "");
  assert.equal(harness.form.resets, 1);
});
