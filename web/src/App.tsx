import { Admin } from './admin/Admin';
import { Scoreboard } from './scoreboard/Scoreboard';

/**
 * The app's root view: the admin console at /admin, the public scoreboard
 * everywhere else. window.location.pathname picks between them; a router is
 * not worth it for two flat views.
 */
function App() {
  const path = window.location.pathname.replace(/\/+$/, '');
  return path === '/admin' ? <Admin /> : <Scoreboard />;
}

export default App;
