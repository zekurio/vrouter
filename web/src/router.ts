// The dashboard's pages and their addresses: / for the overview, and the
// page's name for the others, such as /usage.
import { useEffect, useState, type MouseEvent } from "react";

export const pages = [
  "Overview",
  "Models",
  "Accounts",
  "Keys",
  "Usage",
] as const;
export type Page = (typeof pages)[number];

export const pathOf = (page: Page) =>
  page === "Overview" ? "/" : `/${page.toLowerCase()}`;

// The page an address names. Addresses from before pages had paths named the
// page after a #, so those still open it. Anything else is the overview.
export function pageOf({ pathname, hash }: Location): Page {
  const name = pathname === "/" ? hash.slice(1) : pathname.slice(1);
  return pages.find((p) => p.toLowerCase() === name) ?? "Overview";
}

// A plain click moves within the dashboard. Any other click is left to the
// browser, which opens the address in a new tab or window.
export const plainClick = (e: MouseEvent) =>
  e.button === 0 && !e.metaKey && !e.ctrlKey && !e.shiftKey && !e.altKey;

// The open page, and a way to open another. Each page is an entry in the
// browser's history, so Back returns to the one before.
export function usePage() {
  const [page, setPage] = useState<Page>(() => pageOf(location));
  useEffect(() => {
    // An address that names no page, or names one the old way, becomes the
    // address of the page it opened.
    const opened = pathOf(pageOf(location));
    if (location.pathname !== opened || location.hash !== "")
      history.replaceState(null, "", opened + location.search);
    const change = () => setPage(pageOf(location));
    window.addEventListener("popstate", change);
    return () => window.removeEventListener("popstate", change);
  }, []);
  const navigate = (next: Page) => {
    setPage(next);
    if (pathOf(next) !== location.pathname)
      history.pushState(null, "", pathOf(next));
  };
  return [page, navigate] as const;
}
