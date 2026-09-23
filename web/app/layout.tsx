import type { Metadata, Viewport } from "next";
import type { ReactNode } from "react";

import { Providers } from "@/components/providers";
import { ServiceUnavailable } from "@/components/service-unavailable";
import { SiteHeader } from "@/components/site-header";
import { loadDomain, type DomainLoadResult } from "@/lib/domain";
import { toPublicRuntimeConfig } from "@/lib/runtime-config";
import { themeAttributes } from "@/lib/theme";

import "./globals.css";

// Rendered per request: the domain key and API URL come from the container's env at
// runtime (one image for every domain), never from the build.
export const dynamic = "force-dynamic";

export const viewport: Viewport = {
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#ffffff" },
    { media: "(prefers-color-scheme: dark)", color: "#0a0a0a" },
  ],
};

export async function generateMetadata(): Promise<Metadata> {
  const state = await loadDomain();
  if (!state.ok) return { title: "Service temporarily unavailable" };
  return {
    title: { default: state.domain.name, template: `%s · ${state.domain.name}` },
    description: state.domain.copy.uploadTitle,
  };
}

function developmentDetails(state: Exclude<DomainLoadResult, { ok: true }>): string[] | undefined {
  if (process.env.NODE_ENV !== "development") return undefined;
  return state.reason === "config" ? state.problems : [state.detail];
}

export default async function RootLayout({ children }: { children: ReactNode }) {
  const state = await loadDomain();
  const domainKey = state.ok ? state.config.domainKey : state.reason === "unavailable" ? state.domainKey : null;

  return (
    <html lang="en" {...themeAttributes(domainKey)} className="h-full antialiased">
      <body className="flex min-h-full flex-col">
        <a
          href="#main"
          className="sr-only z-50 rounded-md bg-background px-3 py-2 text-sm font-medium focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:ring-3 focus:ring-ring/50"
        >
          Skip to content
        </a>
        {state.ok ? (
          <Providers config={toPublicRuntimeConfig(state.config)} domain={state.domain}>
            <SiteHeader name={state.domain.name} />
            <main id="main" className="flex w-full flex-1 flex-col">
              {children}
            </main>
          </Providers>
        ) : (
          <>
            <SiteHeader name={null} />
            <main id="main" className="flex w-full flex-1 flex-col">
              <ServiceUnavailable details={developmentDetails(state)} />
            </main>
          </>
        )}
      </body>
    </html>
  );
}
