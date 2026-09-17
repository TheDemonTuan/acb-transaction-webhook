import { QueryClient } from '@tanstack/react-query';
import { ApiError } from '../../api';

export const shouldRetryQuery = (
  failureCount: number,
  error: unknown,
): boolean => {
  if (
    error instanceof ApiError &&
    error.status >= 400 &&
    error.status < 500
  ) {
    return false;
  }

  return failureCount < 1;
};

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 10_000,
      gcTime: 5 * 60 * 1000,
      retry: shouldRetryQuery,
      refetchOnWindowFocus: false,
    },
  },
});
