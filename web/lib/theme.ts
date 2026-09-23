// Per-domain theming. A theme is a CSS-variable set in app/globals.css selected by the
// data-domain attribute on <html>. Unknown domains fall back to the neutral default, so a
// new domain works before it gets its own palette (design phase).

/** Domains with their own palette in app/globals.css (`[data-domain="<key>"]`). */
export const THEMED_DOMAINS = ["cars"] as const;

export type ThemeName = (typeof THEMED_DOMAINS)[number] | "default";

/** Maps a domain key to the theme name used as the <html data-domain> value. */
export function themeForDomain(domainKey: string | null | undefined): ThemeName {
  const key = domainKey?.trim().toLowerCase();
  return (THEMED_DOMAINS as readonly string[]).includes(key ?? "") ? (key as ThemeName) : "default";
}

/** Attributes to spread on <html>. */
export function themeAttributes(domainKey: string | null | undefined): { "data-domain": ThemeName } {
  return { "data-domain": themeForDomain(domainKey) };
}
