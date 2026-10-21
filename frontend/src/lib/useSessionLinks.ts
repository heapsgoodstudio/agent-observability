'use client';
import { useSearchParams } from 'next/navigation';
export function useSessionLinks() {
  const params = useSearchParams();
  return (href: string) => {
    const [path, query] = href.split('?');
    const next = new URLSearchParams(params.toString());
    next.delete('id'); next.delete('tab');
    new URLSearchParams(query).forEach((value, key) => next.set(key, value));
    return `${path}${next.size ? `?${next}` : ''}`;
  };
}
