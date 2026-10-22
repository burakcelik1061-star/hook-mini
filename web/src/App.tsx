import { useEffect } from 'react';
import { keyHint } from './auth';
import { Logo } from './components/ui';
import { Deliveries } from './pages/Deliveries';
import { Endpoints } from './pages/Endpoints';
import { Overview } from './pages/Overview';
import { SendEvent } from './pages/SendEvent';
import { SignIn } from './pages/SignIn';
import { Link, navigate, paths, usePath, type Path } from './router';
import { useSession } from './session';

const nav: { to: Path; label: string }[] = [
  { to: paths.overview, label: 'Overview' },
  { to: paths.endpoints, label: 'Endpoints' },
  { to: paths.deliveries, label: 'Deliveries' },
  { to: paths.send, label: 'Send event' },
];

export function App() {
  const path = usePath();
  const { apiKey } = useSession();

  // Signed out: every screen but sign-in sends you there.
  useEffect(() => {
    if (!apiKey && path !== paths.signIn) navigate(paths.signIn);
    if (apiKey && path === paths.signIn) navigate(paths.overview);
  }, [apiKey, path]);

  useEffect(() => {
    const title = nav.find((n) => n.to === path)?.label ?? 'Sign in';
    document.title = `${title} · Hook Mini`;
  }, [path]);

  if (!apiKey || path === paths.signIn) return <SignIn />;

  return (
    <div className="shell">
      <Header path={path} />
      <main id="main">
        {path === paths.overview && <Overview />}
        {path === paths.endpoints && <Endpoints />}
        {path === paths.deliveries && <Deliveries />}
        {path === paths.send && <SendEvent />}
      </main>
    </div>
  );
}

function Header({ path }: { path: Path }) {
  const { apiKey, rateLimit, signOut } = useSession();
  const left = rateLimit ? Math.round((rateLimit.remaining / rateLimit.limit) * 100) : null;
  return (
    <header className="topbar">
      <a className="visually-hidden" href="#main">
        Skip to content
      </a>
      <Link to={paths.overview} className="brand">
        <Logo />
        Hook Mini
      </Link>
      <nav className="nav" aria-label="Main">
        {nav.map((n) => (
          <Link key={n.to} to={n.to} className="nav-link" aria-current={n.to === path ? 'page' : undefined}>
            {n.label}
          </Link>
        ))}
      </nav>
      <div className="topbar-right">
        {rateLimit && left !== null && (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 6, minWidth: 150 }} title="Requests left in this tenant's burst budget">
            <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12, color: 'var(--muted)' }}>
              <span>Request budget</span>
              <span className="mono" style={{ color: 'var(--text)' }}>
                {rateLimit.remaining} / {rateLimit.limit}
              </span>
            </div>
            <div className="bar" role="meter" aria-label="Request budget left" aria-valuemin={0} aria-valuemax={rateLimit.limit} aria-valuenow={rateLimit.remaining}>
              <span style={{ width: `${left}%`, background: left < 20 ? 'var(--warn)' : 'var(--accent)' }} />
            </div>
          </div>
        )}
        <span className="tag t-dim mono" style={{ padding: '8px 12px' }} title="The API key in use">
          {keyHint(apiKey)}
        </span>
        <button className="btn btn-ghost" type="button" onClick={signOut}>
          Sign out
        </button>
      </div>
    </header>
  );
}
