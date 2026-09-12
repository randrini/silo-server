import { beforeEach, describe, expect, it, vi } from "vitest";
import { createElement, type ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";

const mocks = vi.hoisted(() => ({
  api: vi.fn(),
}));

vi.mock("@/api/client", () => ({
  api: mocks.api,
}));

import {
  fetchCatalogItemDetail,
  fetchCatalogItemEpisodes,
  fetchCatalogItemVersions,
  fetchCatalogSeasonDetail,
  fetchCatalogSeasonEpisodes,
  fetchCatalogSeriesSeasons,
  useCatalogItemDetail,
} from "./catalogRead";

describe("catalog read helpers", () => {
  beforeEach(() => {
    mocks.api.mockReset();
    mocks.api.mockResolvedValue({});
  });

  it("fetches item detail from the canonical catalog item endpoint", async () => {
    await fetchCatalogItemDetail("movie-123");

    expect(mocks.api).toHaveBeenCalledWith("/catalog/items/movie-123", undefined);
  });

  it("encodes item IDs in catalog item endpoints", async () => {
    await fetchCatalogItemDetail("ebook 1/isbn:978", 12);
    await fetchCatalogItemVersions("ebook 1/isbn:978");
    await fetchCatalogItemEpisodes("season 1/id:abc");

    expect(mocks.api).toHaveBeenNthCalledWith(
      1,
      "/catalog/items/ebook%201%2Fisbn%3A978?library_id=12",
      undefined,
    );
    expect(mocks.api).toHaveBeenNthCalledWith(
      2,
      "/catalog/items/ebook%201%2Fisbn%3A978/versions",
      undefined,
    );
    expect(mocks.api).toHaveBeenNthCalledWith(
      3,
      "/catalog/items/season%201%2Fid%3Aabc/episodes",
      undefined,
    );
  });

  it("fetches item episodes from the canonical catalog item episodes endpoint", async () => {
    await fetchCatalogItemEpisodes("season-4");

    expect(mocks.api).toHaveBeenCalledWith("/catalog/items/season-4/episodes", undefined);
  });

  it("fetches series seasons from the canonical catalog series endpoint", async () => {
    await fetchCatalogSeriesSeasons("series-9");

    expect(mocks.api).toHaveBeenCalledWith("/catalog/series/series-9/seasons", undefined);
  });

  it("fetches season detail from the canonical catalog season endpoint", async () => {
    await fetchCatalogSeasonDetail("series-9", 2);

    expect(mocks.api).toHaveBeenCalledWith("/catalog/series/series-9/seasons/2", undefined);
  });

  it("fetches season episodes from the canonical catalog season episodes endpoint", async () => {
    await fetchCatalogSeasonEpisodes("series-9", 2);

    expect(mocks.api).toHaveBeenCalledWith(
      "/catalog/series/series-9/seasons/2/episodes",
      undefined,
    );
  });
});

describe("useCatalogItemDetail caching", () => {
  beforeEach(() => {
    mocks.api.mockReset();
    mocks.api.mockResolvedValue({});
  });

  it("reuses cached item detail when remounting within the stale window", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    mocks.api.mockResolvedValue({ content_id: "movie-1" });
    const wrapper = ({ children }: { children: ReactNode }) =>
      createElement(QueryClientProvider, { client }, children);

    const first = renderHook(() => useCatalogItemDetail("movie-1", 1), { wrapper });
    await waitFor(() => expect(first.result.current.isSuccess).toBe(true));
    expect(mocks.api).toHaveBeenCalledTimes(1);

    // Navigating detail -> player -> back remounts the query. A cache entry
    // inside the 30s stale window must be reused instead of refetched.
    first.unmount();
    const second = renderHook(() => useCatalogItemDetail("movie-1", 1), { wrapper });
    await waitFor(() => expect(second.result.current.isSuccess).toBe(true));

    expect(mocks.api).toHaveBeenCalledTimes(1);
  });
});
