"use strict";

const resourceNames = new Set([
  "blob-stores",
  "cleanup-policies",
  "users",
  "roles",
  "oidc-providers",
  "webhooks",
]);

export function operationsRouteKind(segments) {
  if (segments.length === 1 && segments[0] === "overview") {
    return "overview";
  }
  if (segments[0] !== "tasks") {
    return null;
  }
  if (segments.length === 1) {
    return "task-list";
  }
  return segments.length === 2 ? "task-detail" : null;
}

export function repositoryRouteKind(segments) {
  const [section, repositoryName, child, identifier] = segments;
  if (section === "browse") {
    return segments.length === 1 || segments.length === 2 ? "browse" : null;
  }
  if (section === "search") {
    return segments.length === 1 ? "search" : null;
  }
  if (section !== "repositories") {
    return null;
  }
  if (segments.length === 1) {
    return "repository-list";
  }
  if (segments.length === 2 && repositoryName) {
    return "repository-detail";
  }
  if (segments.length === 3 && child === "assets") {
    return "asset-list";
  }
  if (segments.length === 4 && child === "assets" && identifier) {
    return "asset-detail";
  }
  if (segments.length === 3 && child === "cleanup") {
    return "cleanup";
  }
  return null;
}

export function resourceRouteKind(segments) {
  const [resourceName, identifier, child] = segments;
  if (!resourceNames.has(resourceName)) {
    return null;
  }
  if (segments.length === 1) {
    return "resource-list";
  }
  if (segments.length === 2 && identifier) {
    return "resource-detail";
  }
  if (segments.length === 3 && resourceName === "users" && child === "tokens") {
    return "tokens";
  }
  if (segments.length === 3 && resourceName === "webhooks" && child === "deliveries") {
    return "deliveries";
  }
  return null;
}
