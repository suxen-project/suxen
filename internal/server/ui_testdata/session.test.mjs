import assert from "node:assert/strict";
import test from "node:test";

import {clearSessionCache, endSession, requiresProviderLogout} from "../ui/session.js";

test("logout clears token, provider state, and cached privileges", () => {
  const removed = [];
  const storage = {removeItem: (key) => removed.push(key)};
  const api = {
    token: "secret",
    setToken(value) {
      this.token = value;
    },
  };
  const tokenInput = {value: "secret"};

  const identity = clearSessionCache({
    api,
    storage,
    tokenInput,
    provider: "corporate",
  });

  assert.equal(api.token, "");
  assert.equal(tokenInput.value, "");
  assert.deepEqual(removed, ["suxenOIDCProvider", "suxenOIDC:corporate"]);
  assert.equal(identity.authenticated, false);
  assert.deepEqual(identity.effectivePrivileges, []);
});

test("only an OIDC browser session requires provider cookie logout", () => {
  assert.equal(requiresProviderLogout("oidc-session"), true);
  assert.equal(requiresProviderLogout("oidc-bearer"), false);
  assert.equal(requiresProviderLogout("api-token"), false);
  assert.equal(requiresProviderLogout("anonymous"), false);
});

test("fresh-tab OIDC logout uses the authenticated session provider", async () => {
  const requests = [];
  const storage = {
    getItem: () => null,
  };
  const api = {
    async json(path, options) {
      requests.push({path, options});
    },
  };
  let finalizedProvider = null;

  await endSession({
    identity: {
      authenticated: true,
      authenticationKind: "oidc-session",
      sessionProvider: "corporate/idp",
    },
    storage,
    api,
    finalize: async (provider) => {
      finalizedProvider = provider;
    },
  });

  assert.deepEqual(requests, [{
    path: "/auth/oidc/corporate%2Fidp/logout",
    options: {method: "POST"},
  }]);
  assert.equal(finalizedProvider, "corporate/idp");
});
