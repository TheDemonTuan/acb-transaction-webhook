import { describe, it, expect } from 'vitest';


describe('Cursor pagination navigation model', () => {
  type NavState = {
    pageSize: number;
    cursor: string | undefined;
    pageIndex: number;
    history: (string | undefined)[];
  };

  const createNav = (initialPageSize = 20) => {
    const state: NavState = {
      pageSize: initialPageSize,
      cursor: undefined,
      pageIndex: 0,
      history: [undefined],
    };

    const next = (nextCursor?: string) => {
      if (!nextCursor) return;
      state.pageIndex += 1;
      state.history = state.history.slice(0, state.pageIndex);
      state.history[state.pageIndex] = nextCursor;
      state.cursor = nextCursor;
    };

    const prev = () => {
      if (state.pageIndex <= 0) return;
      state.pageIndex -= 1;
      state.cursor = state.history[state.pageIndex];
    };

    const first = () => {
      state.pageIndex = 0;
      state.cursor = undefined;
      state.history = [undefined];
    };

    const setPageSize = (newSize: number) => {
      state.pageSize = newSize;
      first();
    };

    return {
      get state() {
        return state;
      },
      get pageNumber() {
        return state.pageIndex + 1;
      },
      get hasPrev() {
        return state.pageIndex > 0;
      },
      next,
      prev,
      first,
      setPageSize,
    };
  };

  it('starts at page 1 with no cursor and hasPrev false', () => {
    const nav = createNav();
    expect(nav.pageNumber).toBe(1);
    expect(nav.state.cursor).toBeUndefined();
    expect(nav.hasPrev).toBe(false);
    expect(nav.state.pageSize).toBe(20);
  });

  it('advances through pages and navigates back using cursor history', () => {
    const nav = createNav();

    // Page 1 -> Page 2
    nav.next('cursor_page_2');
    expect(nav.pageNumber).toBe(2);
    expect(nav.state.cursor).toBe('cursor_page_2');
    expect(nav.hasPrev).toBe(true);

    // Page 2 -> Page 3
    nav.next('cursor_page_3');
    expect(nav.pageNumber).toBe(3);
    expect(nav.state.cursor).toBe('cursor_page_3');
    expect(nav.hasPrev).toBe(true);

    // Page 3 -> Page 2
    nav.prev();
    expect(nav.pageNumber).toBe(2);
    expect(nav.state.cursor).toBe('cursor_page_2');
    expect(nav.hasPrev).toBe(true);

    // Page 2 -> Page 1
    nav.prev();
    expect(nav.pageNumber).toBe(1);
    expect(nav.state.cursor).toBeUndefined();
    expect(nav.hasPrev).toBe(false);

    // Page 1 prev is a no-op
    nav.prev();
    expect(nav.pageNumber).toBe(1);
    expect(nav.state.cursor).toBeUndefined();
  });

  it('returns to page 1 on first() and resets history', () => {
    const nav = createNav();
    nav.next('c1');
    nav.next('c2');
    expect(nav.pageNumber).toBe(3);

    nav.first();
    expect(nav.pageNumber).toBe(1);
    expect(nav.state.cursor).toBeUndefined();
    expect(nav.hasPrev).toBe(false);
  });

  it('resets to page 1 when page size changes (20 -> 50 -> 100)', () => {
    const nav = createNav(20);
    nav.next('c1');
    expect(nav.pageNumber).toBe(2);

    nav.setPageSize(50);
    expect(nav.state.pageSize).toBe(50);
    expect(nav.pageNumber).toBe(1);
    expect(nav.state.cursor).toBeUndefined();
    expect(nav.hasPrev).toBe(false);

    nav.next('c50_1');
    expect(nav.pageNumber).toBe(2);

    nav.setPageSize(100);
    expect(nav.state.pageSize).toBe(100);
    expect(nav.pageNumber).toBe(1);
    expect(nav.state.cursor).toBeUndefined();
  });
});

describe('Realtime domain bridge pagination safety', () => {
  it('ensures optimistic prepend matches only first-page queries without cursor', () => {
    // Matches first page (no cursor)
    const page1Query = {
      queryKey: ['transactions', { limit: 20 }],
    };
    // Paginated page 2 (has cursor)
    const page2Query = {
      queryKey: ['transactions', { limit: 20, cursor: 'page2_cursor' }],
    };

    const isFirstPagePredicate = (query: { queryKey: unknown[] }) => {
      const [key, params] = query.queryKey as [string, Record<string, any> | undefined];
      if (key !== 'transactions') return false;
      if (params?.cursor) return false;
      return true;
    };

    const isCursorPagePredicate = (query: { queryKey: unknown[] }) => {
      const [key, params] = query.queryKey as [string, Record<string, any> | undefined];
      if (key !== 'transactions') return false;
      if (!params?.cursor) return false;
      return true;
    };

    expect(isFirstPagePredicate(page1Query)).toBe(true);
    expect(isFirstPagePredicate(page2Query)).toBe(false);

    expect(isCursorPagePredicate(page1Query)).toBe(false);
    expect(isCursorPagePredicate(page2Query)).toBe(true);
  });
});
