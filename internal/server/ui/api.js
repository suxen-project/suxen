"use strict";

import {parseJSON, stringifyJSON} from "./json_codec.js";

export class APIError extends Error {
  constructor(status, message, payload) {
    super(message);
    this.name = "APIError";
    this.status = status;
    this.payload = payload;
  }
}

function isJSONContentType(contentType) {
  return /^application\/(?:[a-z0-9!#$&^_.+-]*\+)?json(?:\s*;|\s*$)/i.test(contentType);
}

class APIClient {
  constructor() {
    this.token = sessionStorage.getItem("suxenToken") || "";
  }

  setToken(token) {
    this.token = token.trim();
    if (this.token) {
      sessionStorage.setItem("suxenToken", this.token);
    } else {
      sessionStorage.removeItem("suxenToken");
    }
  }

  async json(requestPath, options = {}) {
    const headers = new Headers(options.headers || {});
    let body = options.body;
    if (body !== undefined) {
      headers.set("Content-Type", "application/json");
      body = stringifyJSON(body);
    }
    return this.request(requestPath, {...options, headers, body});
  }

  async collection(requestPath) {
    const url = new URL(requestPath, window.location.origin);
    const bounded = url.searchParams.has("limit") || url.searchParams.has("cursor");
    url.searchParams.set("limit", url.searchParams.get("limit") || "200");
    const items = [];
    do {
      const page = await this.json(`${url.pathname}?${url.searchParams.toString()}`);
      items.push(...page.items);
      if (bounded || !page.nextCursor) {
        break;
      }
      url.searchParams.set("cursor", page.nextCursor);
    } while (true);
    return items;
  }

  async page(requestPath, options = {}) {
    const url = new URL(requestPath, window.location.origin);
    url.searchParams.set("limit", String(options.limit || 50));
    // page is random-access numbered pagination; cursor is sequential.
    if (options.page && options.cursor) {
      throw new TypeError("page and cursor cannot be combined");
    }
    if (options.page) {
      url.searchParams.set("page", String(options.page));
    } else if (options.cursor) {
      url.searchParams.set("cursor", options.cursor);
    }
    return this.json(`${url.pathname}?${url.searchParams.toString()}`);
  }

  async upload(requestPath, file, headers = {}) {
    const requestHeaders = new Headers(headers);
    requestHeaders.set("Content-Type", file.type || "application/octet-stream");
    return this.request(requestPath, {
      method: "PUT",
      headers: requestHeaders,
      body: file,
    });
  }

  async download(requestPath, filename) {
    const response = await this.fetch(requestPath, {});
    if (!response.ok) {
      throw await this.responseError(response);
    }
    const blob = await response.blob();
    const objectURL = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = objectURL;
    link.download = filename;
    document.body.append(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(objectURL);
  }

  async request(requestPath, options = {}) {
    const response = await this.fetch(requestPath, options);
    if (!response.ok) {
      throw await this.responseError(response);
    }
    if (response.status === 204) {
      return null;
    }
    const contentType = response.headers.get("Content-Type") || "";
    if (isJSONContentType(contentType)) {
      return parseJSON(await response.text());
    }
    return response.text();
  }

  fetch(requestPath, options) {
    const headers = new Headers(options.headers || {});
    if (this.token) {
      headers.set("Authorization", `Bearer ${this.token}`);
    }
    return fetch(requestPath, {
      ...options,
      headers,
      credentials: "same-origin",
    });
  }

  async responseError(response) {
    const contentType = response.headers.get("Content-Type") || "";
    const payload = isJSONContentType(contentType)
      ? parseJSON(await response.text())
      : await response.text();
    const message = payload?.detail || payload?.message || payload?.reason || payload?.error ||
      payload || response.statusText;
    // A 401 means the session (or token) is no longer accepted. The server sends
    // no WWW-Authenticate: Basic on control-plane routes, so the browser shows no
    // popup; the SPA reacts by re-prompting OIDC login instead.
    if (response.status === 401 && typeof window !== "undefined") {
      window.dispatchEvent(new CustomEvent("suxen:session-expired"));
    }
    return new APIError(response.status, String(message), payload);
  }
}

export const api = new APIClient();

export function apiPath(...segments) {
  return "/" + segments
    .filter((segment) => segment !== undefined && segment !== null && segment !== "")
    .map((segment) => encodeURIComponent(String(segment)))
    .join("/");
}

export function queryString(values) {
  const query = new URLSearchParams();
  for (const [name, value] of Object.entries(values)) {
    if (Array.isArray(value)) {
      for (const item of value) {
        query.append(name, item);
      }
    } else if (value !== undefined && value !== null && value !== "") {
      query.set(name, String(value));
    }
  }
  return query.toString();
}
