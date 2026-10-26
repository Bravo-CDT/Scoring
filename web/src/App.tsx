import { Admin } from './admin/Admin';
import { CredentialsPage } from './credentials/CredentialsPage';
import { Scoreboard } from './scoreboard/Scoreboard';

/**
 * The app's root view: the admin console at /admin, the blue team's
 * credentials page at /credentials, the public scoreboard everywhere else.
 * window.location.pathname picks between them; a router is not worth it for
 * three flat views.
 */
function App() {
  const path = window.location.pathname.replace(/\/+$/, '');
  if (path === '/admin') return <Admin />;
  if (path === '/credentials') return <CredentialsPage />;
  return <Scoreboard />;
}

export default App;
