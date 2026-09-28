"use strict";

import {api} from "./api.js";
import {button, clearNotice, element, routeLink, showNotice} from "./components.js";
import {renderDefaultsRoute} from "./defaults_ui.js";
import {renderOperationsRoute} from "./operations_ui.js";
import {renderRepositoryRoute} from "./repositories.js";
import {renderResourceRoute} from "./resource_views.js";
import {HashRouter, hasPrivilege, LatestRender, routePath} from "./router.js";
import {
  clearSessionCache,
  endSession as endBrowserSession,
  maybeSilentRefresh,
  startOIDCLogin,
} from "./session.js";

const view = document.querySelector("#app-view");
const breadcrumbs = document.querySelector("#breadcrumbs");
const identitySummary = document.querySelector("#identity-summary");
const tokenLogin = document.querySelector("#token-login");
const tokenInput = document.querySelector("#token-input");
const loginProviders = document.querySelector("#login-providers");
const logoutButton = document.querySelector("#logout");
const themeButton = document.querySelector("#theme-toggle");
const navigation = [...document.querySelectorAll("[data-nav]")];

const navigationPrivileges = {
  repositories: "repository:*:read",
  "blob-stores": "admin:blob-stores:read",
  "cleanup-policies": "admin:cleanup-policies:read",
  users: "admin:users:read",
  roles: "admin:roles:read",
  "oidc-providers": "admin:oidc-providers:read",
  webhooks: "admin:webhooks:read",
  defaults: [
    "admin:download-gate-defaults:read",
    "admin:classification-defaults:read",
    "admin:trust-policy-defaults:read",
  ],
  tasks: "admin:tasks:read",
};

let identity = null;
let theme = localStorage.getItem("suxenTheme") || "auto";
const latestRender = new LatestRender();

applyTheme();

themeButton.addEventListener("click", () => {
  const themes = ["auto", "light", "dark"];
  theme = themes[(themes.indexOf(theme) + 1) % themes.length];
  localStorage.setItem("suxenTheme", theme);
  applyTheme();
});

tokenLogin.addEventListener("submit", async (event) => {
  event.preventDefault();
  const token = tokenInput.value.trim();
  if (!token) {
    return;
  }
  api.setToken(token);
  try {
    await loadIdentity();
    if (!identity?.authenticated) {
      throw new Error("The server did not accept this token.");
    }
    tokenInput.value = "";
    showNotice(`Signed in as ${identity.username}.`);
    await renderRouteFromLocation();
  } catch (error) {
    api.setToken("");
    tokenInput.select();
    showNotice(`Token sign-in failed: ${error.message}`, true);
  }
});

logoutButton.addEventListener("click", async () => {
  try {
    await endSession();
    showNotice("Signed out of the administration UI.");
  } catch (error) {
    showNotice(
      `Local session cleared, but provider logout failed: ${error.message}`,
      true,
    );
  }
});

window.addEventListener("suxen:session-ended", (event) => {
  finalizeLocalLogout(event.detail?.provider).then(() => {
    if (event.detail?.remoteSucceeded !== false) {
      showNotice("OIDC session ended.");
    }
  }).catch((error) => {
    showNotice(`Could not render the signed-out state: ${error.message}`, true);
  });
});

window.addEventListener("suxen:resource-changed", () => {
  renderRouteFromLocation().catch((error) => {
    showNotice(`Could not refresh this resource: ${error.message}`, true);
  });
});

window.addEventListener("suxen:session-expired", () => {
  handleSessionExpired();
});

let handlingSessionExpired = false;

// On a 401 from any API call, drop a possibly-stale token and re-derive identity
// from the cookie session. Whoami answers 200 with loginProviders when no
// credentials are supplied, so this ends in the "Sign in with <provider>" state
// rather than another challenge. The guard keeps parallel 401s from stacking.
async function handleSessionExpired() {
  if (handlingSessionExpired) {
    return;
  }
  handlingSessionExpired = true;
  try {
    if (api.token) {
      api.setToken("");
    }
    await loadIdentity();
  } catch {
    identity = clearSessionCache({api, storage: sessionStorage});
    renderIdentity();
    updateNavigation();
  } finally {
    handlingSessionExpired = false;
  }
  if (!identity?.authenticated) {
    showNotice("Your session has expired. Sign in again to continue.", true);
  }
}

function applyTheme() {
  if (theme === "auto") {
    document.documentElement.removeAttribute("data-theme");
  } else {
    document.documentElement.dataset.theme = theme;
  }
  themeButton.textContent = `Theme: ${theme}`;
}

async function loadIdentity() {
  identity = await api.json("/api/v1/whoami");
  renderIdentity();
  updateNavigation();
}

function renderIdentity() {
  if (!identity?.authenticated) {
    identitySummary.textContent = "Anonymous session";
    tokenLogin.hidden = false;
    logoutButton.hidden = !api.token;
    renderLoginProviders(identity?.loginProviders || []);
    return;
  }
  const roles = identity.roles?.length ? ` · ${identity.roles.join(", ")}` : "";
  identitySummary.textContent = `${identity.username} · ${identity.authenticationKind}${roles}`;
  tokenLogin.hidden = true;
  logoutButton.hidden = false;
  renderLoginProviders([]);
}

function renderLoginProviders(providers) {
  loginProviders.replaceChildren(
    ...providers.map((provider) => button(
      `Sign in with ${provider}`,
      () => startOIDCLogin(provider),
      "",
    )),
  );
  loginProviders.hidden = providers.length === 0;
}

function updateNavigation() {
  for (const item of navigation) {
    const required = navigationPrivileges[item.dataset.nav];
    item.hidden = Boolean(required) && !navigationAllowed(required);
  }
}

// A nav entry may require any one of several privileges (the Defaults page reads
// three independent policy defaults); a string requires exactly that privilege.
function navigationAllowed(required) {
  if (Array.isArray(required)) {
    return required.some((privilege) => hasPrivilege(identity, privilege));
  }
  return hasPrivilege(identity, required);
}

async function endSession() {
  await endBrowserSession({
    identity,
    storage: sessionStorage,
    api,
    finalize: finalizeLocalLogout,
  });
}

async function finalizeLocalLogout(provider) {
  identity = clearSessionCache({
    api,
    storage: sessionStorage,
    tokenInput,
    provider,
  });
  renderIdentity();
  updateNavigation();
  window.history.replaceState(null, "", routePath("overview"));
  await renderRouteFromLocation();
}

async function renderRoute(route) {
  // Renew an OIDC session that is about to expire before rendering. This rides
  // the reload along with a navigation the user already made, so an active user
  // stays signed in without a mid-action interruption.
  if (maybeSilentRefresh(identity)) {
    return;
  }
  const rendering = latestRender.begin();
  const routeView = element("div", "route-content");
  clearNotice();
  updateActiveNavigation(route.segments[0]);
  renderBreadcrumbs(route.segments);
  view.setAttribute("aria-busy", "true");
  view.replaceChildren(element("div", "loading", "Loading…"));
  try {
    const handled = await renderOperationsRoute(routeView, route, identity) ||
      await renderRepositoryRoute(routeView, route, identity) ||
      await renderDefaultsRoute(routeView, route, identity) ||
      await renderResourceRoute(routeView, route, identity);
    if (!rendering.isCurrent()) {
      return;
    }
    if (!handled) {
      renderNotFound(routeView, route);
    }
    view.replaceChildren(routeView);
  } catch (error) {
    if (!rendering.isCurrent()) {
      return;
    }
    if (error?.status === 401) {
      routeView.replaceChildren(
        element("h1", "", "Sign in required"),
        element("p", "", "Your session has expired. Use an API token or provider button above to sign in."),
      );
      view.replaceChildren(routeView);
      return;
    }
    renderError(error);
  } finally {
    if (rendering.isCurrent()) {
      view.removeAttribute("aria-busy");
    }
  }
}

function updateActiveNavigation(section) {
  for (const item of navigation) {
    item.classList.toggle("active", item.dataset.nav === section);
    if (item.dataset.nav === section) {
      item.setAttribute("aria-current", "page");
    } else {
      item.removeAttribute("aria-current");
    }
  }
}

function renderBreadcrumbs(segments) {
  breadcrumbs.replaceChildren(routeLink("Overview", routePath("overview")));
  if (segments[0] === "overview") {
    return;
  }
  segments.forEach((segment, index) => {
    breadcrumbs.append(element("span", "breadcrumb-separator", "/"));
    const label = humanize(segment);
    if (index === segments.length - 1) {
      breadcrumbs.append(element("span", "", label));
    } else {
      breadcrumbs.append(routeLink(label, routePath(...segments.slice(0, index + 1))));
    }
  });
}

function renderNotFound(container, route) {
  container.replaceChildren(
    element("h1", "", "Page not found"),
    element("p", "", `No UI route handles ${route.segments.join(" / ")}.`),
    routeLink("Return to overview", routePath("overview"), "button"),
  );
}

function renderError(error) {
  view.replaceChildren(
    element("h1", "", "Could not load this page"),
    element("p", "inline-error", error.message),
    routeLink("Return to overview", routePath("overview"), "button secondary"),
  );
  showNotice(error.message, true);
}

function humanize(value) {
  return value.replaceAll("-", " ").replace(/^./, (letter) => letter.toUpperCase());
}

async function renderRouteFromLocation() {
  await router.onChange();
}

const router = new HashRouter(renderRoute);

try {
  await loadIdentity();
} catch (error) {
  identity = clearSessionCache({
    api,
    storage: sessionStorage,
  });
  renderIdentity();
  updateNavigation();
  showNotice(`Identity discovery failed: ${error.message}`, true);
}
router.start();
