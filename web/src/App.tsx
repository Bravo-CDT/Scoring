import { Admin } from './admin/Admin';
import { CredentialsPage } from './credentials/CredentialsPage';
import { InjectsPage } from './injects/InjectsPage';
import { Scoreboard } from './scoreboard/Scoreboard';

/**
 * The app's root view: the admin console at /admin, the blue team's
 * credentials page at /credentials, the teams' injects page at /injects, the
 * public scoreboard everywhere else. window.location.pathname picks between
 * them; a router is not worth it for four flat views.
 */
function App() {
  const path = window.location.pathname.replace(/\/+$/, '');
  if (path === '/admin') return <Admin />;
  if (path === '/credentials') return <CredentialsPage />;
  if (path === '/injects') return <InjectsPage />;
  return <Scoreboard />;
}

export default App;
