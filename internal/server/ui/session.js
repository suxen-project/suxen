"use strict";

export function anonymousIdentity() {
  return {
    authenticated: false,
    admin: false,
    authenticationKind: "anonymous",
    roles: ["anonymous"],
    effectivePrivileges: [],
  };
}

export function clearSessionCache({api, storage, tokenInput, provider}) {
  api.setToken("");
  if (tokenInput) {
    tokenInput.value = "";
  }
  storage.removeItem("suxenOIDCProvider");
  if (provider) {
    storage.removeItem(`suxenOIDC:${provider}`);
  }
  return anonymousIdentity();
}

export function requiresProviderLogout(authenticationKind) {
  return authenticationKind === "oidc-session";
}

export async function endSession({identity, storage, api, finalize}) {
  const authenticationKind = identity?.authenticationKind;
  const provider = identity?.sessionProvider || storage.getItem("suxenOIDCProvider");
  let logoutError = null;
  try {
    if (requiresProviderLogout(authenticationKind)) {
      if (!provider) {
        throw new Error(
          "This OIDC session does not record its provider; use the provider logout action.",
        );
      }
      await api.json(`/auth/oidc/${encodeURIComponent(provider)}/logout`, {method: "POST"});
    }
  } catch (error) {
    logoutError = error;
  }
  await finalize(provider);
  if (logoutError) {
    throw logoutError;
  }
}

// The provider name is remembered so logout can end the session at the same provider;
// the current hash route rides along so the callback returns to the page being viewed.
export function oidcLoginURL(provider, hash = "") {
  const redirect = encodeURIComponent(`/ui/${hash}`);
  return `/auth/oidc/${encodeURIComponent(provider)}/login?redirect=${redirect}`;
}

// prompt=none tells the provider to renew the session against its existing login
// without any interaction, so the redirect is invisible when the IdP session is
// still alive and returns login_required (handled server-side) otherwise.
export function silentOIDCLoginURL(provider, hash = "") {
  return `${oidcLoginURL(provider, hash)}&prompt=none`;
}

export function startOIDCLogin(provider, {storage, location} = {}) {
  const store = storage || sessionStorage;
  const target = location || window.location;
  store.setItem("suxenOIDCProvider", provider);
  target.assign(oidcLoginURL(provider, target.hash));
}

// Renew an OIDC browser session within this many seconds of its expiry, so an
// active user never hits a hard 401 mid-session.
const SILENT_REFRESH_WINDOW_SECONDS = 120;

export function silentRefreshDue(identity, {now = Date.now()} = {}) {
  if (identity?.authenticationKind !== "oidc-session") {
    return false;
  }
  const expiresAt = Number(identity?.expiresAt);
  if (!identity?.sessionProvider || !Number.isFinite(expiresAt) || expiresAt <= 0) {
    return false;
  }
  const remaining = expiresAt - Math.floor(now / 1000);
  return remaining > 0 && remaining <= SILENT_REFRESH_WINDOW_SECONDS;
}

// maybeSilentRefresh navigates to a prompt=none re-auth when the session is near
// expiry, returning true when it does (the caller should stop rendering — the
// page is leaving). It is triggered on navigation, so the unavoidable reload
// rides along with a route change the user already made rather than interrupting
// them. The attempt is keyed on the session's expiry: a success returns a fresh,
// far-off expiry (so this does not fire again), and a login_required leaves the
// same expiry recorded (so it is not retried until the session truly lapses and
// the 401 path shows a visible login) — no redirect loop, no popup.
export function maybeSilentRefresh(identity, {storage, location, now} = {}) {
  if (typeof document !== "undefined" && document.visibilityState === "hidden") {
    return false;
  }
  if (!silentRefreshDue(identity, {now})) {
    return false;
  }
  const store = storage || sessionStorage;
  const target = location || window.location;
  const key = String(identity.expiresAt);
  if (store.getItem("suxenSilentRefreshExp") === key) {
    return false;
  }
  store.setItem("suxenSilentRefreshExp", key);
  store.setItem("suxenOIDCProvider", identity.sessionProvider);
  target.assign(silentOIDCLoginURL(identity.sessionProvider, target.hash));
  return true;
}
