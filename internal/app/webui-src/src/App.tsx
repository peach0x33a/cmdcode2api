import { useCallback, useState } from "react";
import Accounts from "./Accounts";
import Models from "./Models";
import Setup from "./Setup";
import Tabs, { TAB_ORDER, type TabId } from "./Tabs";
import Usage from "./Usage";

function tabFromHash(hash: string): TabId {
  const id = hash.replace(/^#/, "");
  return (TAB_ORDER as string[]).includes(id) ? (id as TabId) : "accounts";
}

export default function App() {
  const [active, setActive] = useState<TabId>(() => tabFromHash(window.location.hash));

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
      {active === "setup" ? <Setup /> : null}

      <footer>
        Served from the gateway itself — this page only works when opened from
        this machine (127.0.0.1/localhost).
      </footer>
    </div>
  );
}
