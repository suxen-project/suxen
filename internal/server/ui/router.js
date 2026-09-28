"use strict";

export function encodeSegment(value) {
  return encodeURIComponent(String(value));
}

export function routePath(...segments) {
  return "#/" + segments.filter((segment) => segment !== "").map(encodeSegment).join("/");
}

export function parseHash(hash) {
  const value = String(hash || "").replace(/^#\/?/, "");
  if (!value) {
    return {segments: ["overview"], query: new URLSearchParams()};
  }
  const [path, query = ""] = value.split("?", 2);
  const segments = path.split("/").filter(Boolean).map((segment) => decodeURIComponent(segment));
  return {segments: segments.length ? segments : ["overview"], query: new URLSearchParams(query)};
}

export class HashRouter {
  constructor(render) {
    this.render = render;
    this.onChange = this.onChange.bind(this);
  }

  start() {
    window.addEventListener("hashchange", this.onChange);
    if (!window.location.hash) {
      window.location.replace("#/overview");
      return;
    }
    this.onChange();
  }

  async onChange() {
    await this.render(parseHash(window.location.hash));
  }
}

export class LatestRender {
  constructor() {
    this.sequence = 0;
  }

  begin() {
    const sequence = ++this.sequence;
    return {
      isCurrent: () => sequence === this.sequence,
    };
  }
}

export function privilegeMatches(granted, required) {
  if (granted === "*") {
    return true;
  }
  const grantedParts = granted.split(":");
  const requiredParts = required.split(":");
  const length = Math.max(grantedParts.length, requiredParts.length);
  for (let index = 0; index < length; index += 1) {
    const grantedPart = grantedParts[index] ?? grantedParts.at(-1);
    const requiredPart = requiredParts[index] ?? requiredParts.at(-1);
    if (grantedPart !== "*" && requiredPart !== "*" && grantedPart !== requiredPart) {
      return false;
    }
  }
  return true;
}

export function hasPrivilege(identity, required) {
  return Boolean(identity?.effectivePrivileges?.some((granted) => privilegeMatches(granted, required)));
}

export function canLoadPrivilegeCatalog(identity) {
  return hasPrivilege(identity, "admin:privileges:read");
}
