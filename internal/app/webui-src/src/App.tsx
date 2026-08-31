import { useCallback, useEffect, useState } from "react";
import { fetchVersion } from "./api";
import Accounts from "./Accounts";
import Models from "./Models";
import Setup from "./Setup";
import Tabs, { TAB_ORDER, type TabId } from "./Tabs";
import Usage from "./Usage";
import Monitoring from "./Monitoring";
import Alerts from "./Alerts";

function tabFromHash(hash: string): TabId {
  const id = hash.replace(/^#/, "");
  return (TAB_ORDER as string[]).includes(id) ? (id as TabId) : "accounts";
}

export default function App() {
  const [active, setActive] = useState<TabId>(() => tabFromHash(window.location.hash));
  const [version, setVersion] = useState("");

  useEffect(() => {
    fetchVersion().then(setVersion);
  }, []);

  const handleSelect = useCallback((id: TabId) => {
    setActive(id);
    window.location.hash = id;
  }, []);

  return (
    <div>
      <h1>cmdcode2api</h1>

      <Tabs active={active} onSelect={handleSelect} />

      {active === "accounts" ? <Accounts /> : null}
      {active === "models" ? <Models /> : null}
      {active === "usage" ? <Usage /> : null}
      {active === "monitor" ? <Monitoring /> : null}
      {active === "alerts" ? <Alerts /> : null}
      {active === "setup" ? <Setup /> : null}

      <footer>
        Served from the gateway itself. Opened from this machine
        (127.0.0.1/localhost), it just works; from another device on your
        network or Tailscale, it needs the gateway's API key to unlock.
        {version ? <span className="footer-version"> · {version}</span> : null}
      </footer>
    </div>
  );
}
