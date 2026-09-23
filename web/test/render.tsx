import { QueryClient } from "@tanstack/react-query";
import { render, type RenderOptions } from "@testing-library/react";
import type { ReactElement } from "react";

import { Providers } from "@/components/providers";
import type { Api, PublicDomain } from "@/lib/api";

import { API_BASE, carsDomain } from "./fixtures";

export function testQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { gcTime: Infinity }, mutations: { retry: false } },
  });
}

export interface ProviderOptions {
  api?: Api;
  domain?: PublicDomain;
  queryClient?: QueryClient;
}

/** Renders inside <Providers> with the cars domain and a test API base URL. */
export function renderWithProviders(ui: ReactElement, opts: ProviderOptions & Omit<RenderOptions, "wrapper"> = {}) {
  const { api, domain = carsDomain, queryClient = testQueryClient(), ...rest } = opts;
  return {
    queryClient,
    ...render(ui, {
      wrapper: ({ children }) => (
        <Providers config={{ domainKey: domain.key, apiUrl: API_BASE }} domain={domain} api={api} queryClient={queryClient}>
          {children}
        </Providers>
      ),
      ...rest,
    }),
  };
}
