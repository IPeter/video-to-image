"use client";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createContext, useContext, useMemo, useState, type ReactNode } from "react";

import { createApi, type Api, type PublicDomain } from "@/lib/api";
import type { PublicRuntimeConfig } from "@/lib/runtime-config-parse";

// Client-side context for everything the server already resolved per request: the
// runtime config (domain key, browser-facing API URL) and the public domain config.
// Client components read it from here instead of fetching /api/config.

export interface AppContextValue {
  config: PublicRuntimeConfig;
  domain: PublicDomain;
  api: Api;
}

const AppContext = createContext<AppContextValue | null>(null);

export function makeQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 0,
        refetchOnWindowFocus: true,
      },
      mutations: { retry: false },
    },
  });
}

export interface ProvidersProps {
  config: PublicRuntimeConfig;
  domain: PublicDomain;
  children: ReactNode;
  /** For tests. */
  queryClient?: QueryClient;
  /** For tests: replaces the API helpers built from config.apiUrl. */
  api?: Api;
}

export function Providers({ config, domain, children, queryClient, api }: ProvidersProps) {
  const [client] = useState(() => queryClient ?? makeQueryClient());
  const value = useMemo<AppContextValue>(
    () => ({ config, domain, api: api ?? createApi(config.apiUrl) }),
    [config, domain, api],
  );
  return (
    <QueryClientProvider client={client}>
      <AppContext.Provider value={value}>{children}</AppContext.Provider>
    </QueryClientProvider>
  );
}

export function useAppContext(): AppContextValue {
  const ctx = useContext(AppContext);
  if (!ctx) throw new Error("useAppContext must be used inside <Providers>");
  return ctx;
}

export const useRuntimeConfig = (): PublicRuntimeConfig => useAppContext().config;
export const useDomain = (): PublicDomain => useAppContext().domain;
export const useApi = (): Api => useAppContext().api;
