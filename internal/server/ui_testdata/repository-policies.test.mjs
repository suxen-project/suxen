import assert from "node:assert/strict";
import test from "node:test";

// The repository view uses only these DOM operations to render its detail panel.
// Keep the real route, API adapter, and component rendering in this regression.
class TestNode {
  constructor(tagName = "", text = "") {
    this.tagName = tagName;
    this.children = [];
    this.text = text;
    this.classList = {add() {}, toggle() {}};
  }

  set textContent(value) { this.text = value; this.children = []; }
  get textContent() { return this.text + this.children.map((child) => child.textContent).join(""); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = children; }
  addEventListener() {}
}

test("repository policy editing requires manage and group gates are enforced on members", async (context) => {
  globalThis.Node = TestNode;
  globalThis.sessionStorage = {getItem: () => null};
  globalThis.document = {
    querySelector: () => new TestNode(),
    createElement: (tagName) => new TestNode(tagName),
    createTextNode: (text) => new TestNode("#text", text),
  };
  context.after(() => {
    delete globalThis.Node;
    delete globalThis.sessionStorage;
    delete globalThis.document;
  });
  const {api, APIError} = await import("../ui/api.js");
  const {renderRepositoryRoute} = await import("../ui/repositories.js");
  const identity = {effectivePrivileges: ["repository:*:read", "repository:*:manage"]};
  const descendants = (node) => [node, ...node.children.flatMap(descendants)];

  for (const type of ["group", "hosted", "proxy"]) {
    const requests = [];
    const repository = {name: "packages", format: "raw", type, members: ["member"]};
    const stub = context.mock.method(api, "json", async (path) => {
      requests.push(path);
      if (path === "/api/v1/repositories/packages") {
        return repository;
      }
      throw new APIError(404, "Not configured");
    });
    const view = new TestNode();
    await renderRepositoryRoute(view, {segments: ["repositories", "packages"]}, identity);
    const buttons = descendants(view).filter((node) => node.tagName === "button");
    assert.equal(buttons.some((node) => node.textContent === "Trust policy"), type !== "group");
    assert.equal(buttons.some((node) => node.textContent === "Download gate"), type !== "group");
    assert.equal(requests.includes("/api/v1/repositories/packages/download-gate"), type !== "group");
    assert.equal(requests.includes("/api/v1/repositories/packages/trust-policy"), type !== "group");
    if (type === "group") {
      assert.match(view.textContent, /Trust policyEnforced by member repositories \(including their instance defaults\)/);
      assert.match(view.textContent, /Download gateEnforced by member repositories \(including their instance defaults\)/);
    } else {
      assert.match(view.textContent, /Trust policyNot configured \(inherits instance default\)/);
    }
    stub.mock.restore();
  }

  const annotateView = new TestNode();
  context.mock.method(api, "json", async (path) => path === "/api/v1/repositories/packages"
    ? {name: "packages", format: "raw", type: "hosted"}
    : Promise.reject(new APIError(404, "Not configured")));
  await renderRepositoryRoute(annotateView, {segments: ["repositories", "packages"]},
    {effectivePrivileges: ["repository:*:read", "repository:*:annotate"]});
  const annotateButtons = descendants(annotateView).filter((node) => node.tagName === "button");
  assert.equal(annotateButtons.some((node) => ["Classification", "Download gate", "Trust policy"].includes(node.textContent)), false);
});

test("group asset drill-in shows its manifest without member mutation actions", async (context) => {
  globalThis.Node = TestNode;
  globalThis.sessionStorage = {getItem: () => null};
  globalThis.document = {
    querySelector: () => new TestNode(),
    createElement: (tagName) => new TestNode(tagName),
    createTextNode: (text) => new TestNode("#text", text),
  };
  context.after(() => {
    delete globalThis.Node;
    delete globalThis.sessionStorage;
    delete globalThis.document;
  });
  const {api} = await import("../ui/api.js");
  const {renderRepositoryRoute} = await import("../ui/repositories.js");
  const requests = [];
  context.mock.method(api, "json", async (path) => {
    requests.push(path);
    if (path === "/api/v1/repositories/oci-group") {
      return {name: "oci-group", format: "oci", type: "group"};
    }
    if (path === "/api/v1/repositories/oci-group/assets/42") {
      return {id: 42, repository: "oci-group", path: "v2/acme/widget/manifests/stable",
        digest: "sha256:manifest", size: 12, kind: "oci-manifest", attributes: {notes: {owner: "team"}}};
    }
    if (path === "/api/v1/repositories/oci-group/assets/42/manifest") {
      return {mediaType: "application/vnd.oci.image.manifest.v1+json", layers: []};
    }
    throw new Error(`unexpected request: ${path}`);
  });
  const view = new TestNode();
  await renderRepositoryRoute(view,
    {segments: ["repositories", "oci-group", "assets", "42"]},
    {effectivePrivileges: ["repository:*:read", "repository:*:annotate", "repository:*:delete"]});
  assert.ok(requests.includes("/api/v1/repositories/oci-group/assets/42/manifest"));
  const descendants = (node) => [node, ...node.children.flatMap(descendants)];
  const buttons = descendants(view).filter((node) => node.tagName === "button").map((node) => node.textContent);
  assert.deepEqual(buttons, ["Download"]);
  assert.match(view.textContent, /Manifest contents/);
});
