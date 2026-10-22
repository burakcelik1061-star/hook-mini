// A small router over the History API: the console has five fixed screens,
// which does not need a library. The Go side serves the app on exactly these
// paths (internal/webui.Routes).

import { useEffect, useState, type AnchorHTMLAttributes, type MouseEvent } from 'react';

export const paths = {
  overview: '/',
  signIn: '/sign-in',
  endpoints: '/endpoints',
  deliveries: '/deliveries',
  send: '/send',
} as const;

export type Path = (typeof paths)[keyof typeof paths];

const known = new Set<string>(Object.values(paths));

function current(): Path {
  const p = window.location.pathname;
  return (known.has(p) ? p : paths.overview) as Path;
}

export function navigate(to: Path, query = ''): void {
  const target = to + query;
  if (window.location.pathname + window.location.search === target) return;
  window.history.pushState(null, '', target);
  window.dispatchEvent(new PopStateEvent('popstate'));
}

export function usePath(): Path {
  const [path, setPath] = useState<Path>(current);
  useEffect(() => {
    const on = () => setPath(current());
    window.addEventListener('popstate', on);
    return () => window.removeEventListener('popstate', on);
  }, []);
  return path;
}

export function useQuery(name: string): string | null {
  const path = usePath();
  void path; // re-read on navigation
  return new URLSearchParams(window.location.search).get(name);
}

type LinkProps = Omit<AnchorHTMLAttributes<HTMLAnchorElement>, 'href'> & { to: Path; query?: string };

/** An <a> that navigates without a reload; modified clicks keep their meaning. */
export function Link({ to, query = '', onClick, ...rest }: LinkProps) {
  return (
    <a
      {...rest}
      href={to + query}
      onClick={(e: MouseEvent<HTMLAnchorElement>) => {
        onClick?.(e);
        if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
        e.preventDefault();
        navigate(to, query);
      }}
    />
  );
}
