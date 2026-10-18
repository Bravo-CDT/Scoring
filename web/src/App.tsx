import { Scoreboard } from './scoreboard/Scoreboard';

/**
 * The app's root view. The public scoreboard is its only view so far; when
 * more arrive, window.location.pathname picks between them (a router is not
 * worth it for a couple of flat views).
 */
function App() {
  return <Scoreboard />;
}

export default App;
