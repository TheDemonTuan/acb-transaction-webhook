import React, { useState, useCallback, useRef } from 'react';
import { ChevronLeft, ChevronRight, ChevronsLeft } from 'lucide-react';

export function useCursorPagination(initialPageSize = 20) {
  const [pageSize, setPageSizeState] = useState<number>(initialPageSize);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [pageIndex, setPageIndex] = useState<number>(0);
  const historyRef = useRef<(string | undefined)[]>([undefined]);

  const reset = useCallback(() => {
    historyRef.current = [undefined];
    setCursor(undefined);
    setPageIndex(0);
  }, []);

  const setPageSize = useCallback((newSize: number) => {
    setPageSizeState(newSize);
    historyRef.current = [undefined];
    setCursor(undefined);
    setPageIndex(0);
  }, []);

  const handleNext = useCallback((nextCursor?: string) => {
    if (!nextCursor) return;
    setPageIndex((prevIndex) => {
      const nextIndex = prevIndex + 1;
      historyRef.current = historyRef.current.slice(0, nextIndex);
      historyRef.current[nextIndex] = nextCursor;
      setCursor(nextCursor);
      return nextIndex;
    });
  }, []);

  const handlePrev = useCallback(() => {
    setPageIndex((prevIndex) => {
      if (prevIndex <= 0) return 0;
      const targetIndex = prevIndex - 1;
      setCursor(historyRef.current[targetIndex]);
      return targetIndex;
    });
  }, []);

  const handleFirst = useCallback(() => {
    reset();
  }, [reset]);

  return {
    pageSize,
    cursor,
    pageIndex,
    pageNumber: pageIndex + 1,
    hasPrev: pageIndex > 0,
    canFirst: pageIndex > 0,
    reset,
    setPageSize,
    handleNext,
    handlePrev,
    handleFirst,
  };
}

export interface PaginationControlsProps {
  pageNumber: number;
  itemCount: number;
  pageSize: number;
  pageSizeOptions?: number[];
  hasNext: boolean;
  hasPrev: boolean;
  isLoading?: boolean;
  onNext: () => void;
  onPrev: () => void;
  onFirst: () => void;
  onPageSizeChange: (newSize: number) => void;
}

export const PaginationControls: React.FC<PaginationControlsProps> = ({
  pageNumber,
  itemCount,
  pageSize,
  pageSizeOptions = [20, 50, 100],
  hasNext,
  hasPrev,
  isLoading = false,
  onNext,
  onPrev,
  onFirst,
  onPageSizeChange,
}) => {
  return (
    <div className="p-4 border-t border-stone-100 flex flex-col sm:flex-row items-center justify-between gap-3 bg-stone-50/50">
      <div className="flex items-center gap-3 text-xs text-stone-500">
        <span>
          Trang <strong className="text-stone-800">{pageNumber}</strong> • {itemCount} dòng
        </span>
        <span className="text-stone-300">|</span>
        <div className="flex items-center gap-1.5">
          <span className="text-stone-500">Hiển thị:</span>
          <select
            value={pageSize}
            onChange={(e) => onPageSizeChange(Number(e.target.value))}
            disabled={isLoading}
            aria-label="Số dòng mỗi trang"
            className="px-2 py-1 bg-white border border-stone-200 rounded-lg text-xs text-stone-700 font-medium focus:outline-none focus:ring-1 focus:ring-stone-400 cursor-pointer disabled:opacity-50"
          >
            {pageSizeOptions.map((opt) => (
              <option key={opt} value={opt}>
                {opt} / trang
              </option>
            ))}
          </select>
        </div>
      </div>

      <div className="flex items-center gap-1.5">
        <button
          type="button"
          onClick={onFirst}
          disabled={!hasPrev || isLoading}
          className="inline-flex items-center gap-1 px-2.5 py-1.5 rounded-xl text-xs font-semibold bg-white border border-stone-200 text-stone-700 hover:bg-stone-50 transition shadow-2xs cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed"
          title="Trang đầu"
        >
          <ChevronsLeft className="w-3.5 h-3.5" />
          <span className="hidden sm:inline">Trang đầu</span>
        </button>
        <button
          type="button"
          onClick={onPrev}
          disabled={!hasPrev || isLoading}
          className="inline-flex items-center gap-1 px-3 py-1.5 rounded-xl text-xs font-semibold bg-white border border-stone-200 text-stone-700 hover:bg-stone-50 transition shadow-2xs cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed"
        >
          <ChevronLeft className="w-3.5 h-3.5" />
          <span>Trước</span>
        </button>
        <button
          type="button"
          onClick={onNext}
          disabled={!hasNext || isLoading}
          className="inline-flex items-center gap-1 px-3 py-1.5 rounded-xl text-xs font-semibold bg-white border border-stone-200 text-stone-700 hover:bg-stone-50 transition shadow-2xs cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed"
        >
          <span>Sau</span>
          <ChevronRight className="w-3.5 h-3.5" />
        </button>
      </div>
    </div>
  );
};
