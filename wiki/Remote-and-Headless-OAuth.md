# Remote & Headless OAuth

The OAuth flow waits for a callback on `http://localhost:5959/callback`, and
Command Code only allows a `localhost` callback. When the gateway runs on a
server without a browser, forward port 5959 over SSH from a machine that has one
(Windows included) so that `localhost` callback still reaches the server.

1. From your workstation, open an SSH session that forwards the callback port,
   and leave it open for the whole flow:

   ```bash
   ssh -L 5959:localhost:5959 user@server
   ```

2. With the gateway running on the server, open its `/ui` in your workstation
   browser (through the tunnel or over LAN / Tailscale) and click **Add
   account** or **Reauthorize**. It binds `127.0.0.1:5959` on the server and
   opens an authorization URL.

   From the command line instead: `./cmdcode2api --oauth --account personal`,
   which prints the URL rather than opening it.

3. Approve in the browser on your workstation. The callback travels back through
   the tunnel; the key is written into `config.yaml`.

4. Close the SSH session (`exit`). An account added through the UI is live
   immediately; one added with the `--oauth` command needs a gateway restart to
   load.

Port 5959 is fixed. Don't pass `--oauth-callback` for the tunnel case — the
default `localhost` callback is what makes it work. Use `--oauth-callback` only
when something other than `localhost:5959` must receive the callback, for
example a public HTTPS reverse proxy.
