# GossipWire

GossipWire is a Discord bot that lets moderators of linked servers warn each other about spammers and scammers, and ban them with one button, for small community networks that already trust each other.

An alert is a claim from one server's mod. The mods in the receiving server decide whether to act on it. The bot never bans anyone on its own.

## Prerequisites

- A machine with Docker (with the compose plugin) or Podman (with podman-compose). A homelab box or a small VPS is plenty.
- A Discord account.
- A Discord application with a bot token. The steps below walk through making one.
- For setup C only: a domain name pointed at your machine, and a reverse proxy that does https (Nginx Proxy Manager, Caddy, Traefik, or whichever you use).

## Pick your setup

Most people want setup A. Pick the row that matches what you want, then use those values in `compose.yml`.

| Setup | You want | `FEDERATION_MODE` | `INSTANCE_URL` | Ports or proxy |
| --- | --- | --- | --- | --- |
| A. Just my servers | Alerts between servers that use your bot. Nothing else. | `off` | leave empty | none |
| B. Home connection, talk to others | Also trade alerts with servers on someone else's instance, from a home box with no open ports | `allowlist` | leave empty | none |
| C. Public, talk to others | Other instances can reach yours directly | `allowlist` or `open` | your public https address | reverse proxy with TLS |

Setup B needs at least one peer that is setup C, since two instances with no open ports cannot reach each other.

You can move from A to B or C later. Moving away from C, or changing `INSTANCE_URL`, breaks every cross-instance subscription, since other servers know yours by that address.

## Setting it up

### Create the Discord application

1. Go to https://discord.com/developers/applications and sign in.
2. Click **New Application**, name it (GossipWire, or whatever your mods will recognize), tick the terms box, and click **Create**.
3. In the left menu, click **Bot**.
4. Under **Token**, click **Reset Token**, confirm, and copy the token it shows. You only see it once. Keep it in your clipboard for step 10.
5. Still on the Bot page, leave all three **Privileged Gateway Intents** switched off, since GossipWire does not need any of them.
5.5. Still on the Bot page, turn **Public Bot** off if only your own servers will ever add this bot. Leave it on if other people's servers will add it from the install link.
6. In the left menu, click **Installation**.
7. Under **Installation Contexts**, untick **User Install** and leave **Guild Install** ticked, since the bot only works inside a server.
8. Set **Install Link** to **Discord Provided Link**, since that is what puts the **Add App** button on the bot's profile.
9. Under **Default Install Settings**, for Guild Install pick the scopes `bot` and `applications.commands`, and the permissions **View Channels**, **Send Messages**, **Embed Links**, **Attach Files** and **Ban Members**. Click **Save Changes**.

NEVER SHARE THE BOT TOKEN. Not in a screenshot, not in a chat, not in a support thread, not in `compose.yml`. Whoever has it IS your bot, with ban power in every server it is in. If it leaks, go back to step 4 and reset it.

### Get the code and fill in the settings

I run Docker, so in my case the commands are `docker compose`. On Podman it's `podman-compose`, or whichever you use.

10. Clone the repo and step into it: `git clone https://github.com/NullAngst/GossipWire.git && cd GossipWire`
11. Make the token file, pasting your token where it says: `printf '%s\n' 'PASTE-TOKEN-HERE' > discord_token.txt`
12. Make the data folder: `mkdir data`
13. Give both to the container's user: `sudo chown 65532:65532 data discord_token.txt`, since the bot runs as UID 65532 and has to read the token and write its database. On rootless Podman, skip this and change the volume line to `./data:/data:U`, then run `podman unshare chown 65532:65532 discord_token.txt`.
14. Open `compose.yml` in nano, or whichever editor you prefer: `nano compose.yml`
15. Set `OWNER_ID` to your own Discord user ID. Turn on Developer Mode in Discord (Settings > Advanced), right-click your name anywhere, and pick **Copy User ID**. The example `123456789012345678` is rejected on purpose.
16. Set `FEDERATION_MODE` to the value from your row in Pick your setup. If you are not sure, leave it at `off`.
17. Setup C only: set `INSTANCE_URL` to your public address, like `https://alerts.yourdomain.net`, with no slash at the end, and remove the `#` from the two `ports:` lines. Then point your reverse proxy at `http://127.0.0.1:8081` with a TLS certificate. If your proxy runs in Docker too, read Reverse proxy in Docker below instead of using `ports:`.
18. Save the file.

DO NOT CHANGE `INSTANCE_URL` ONCE PEERS KNOW YOU. Other instances know your servers by that address, and changing it breaks every subscription they have with you.

### Start it

19. Build the bot: `docker compose build --pull`. The first build takes a minute or two, since it compiles the bot. `--pull` fetches the newest Go and base images instead of reusing old copies Docker has cached.
19.5. Start it: `docker compose up -d`
20. Check the log: `docker compose logs -f gossipwire`. A clean start ends with a line like `GossipWire is running. Setup A: just my servers. Owner ... Install link: https://discord.com/oauth2/authorize?...`. If it says `GossipWire cannot start:` instead, the message tells you which line to fix, and Troubleshooting below has every one of them.
21. Open the install link from the log, pick your server, and click **Authorize**. Other admins add it the same way, from that link or from the **Add App** button on the bot's profile.
22. In Server Settings > Roles, drag the GossipWire role above the roles of anyone it might need to ban, since Discord only lets a bot ban users whose highest role sits below its own.
23. In your mod channel, run `/setup channel:#mod-alerts` (your channel name will differ). That channel is where your alerts go out from and other servers' alerts come in to. Use a channel only your mods can see, since alert cards carry user IDs, reasons and evidence from other servers.
24. Run `/instance status`. It should name the setup you picked in the same words as the table above.

BACK UP THE DATA FOLDER. It holds the database, the evidence files and the instance's identity key. Lose it and every subscription is gone, and other instances see your bot as a stranger with a new key.

### Link servers

25. To receive another server's alerts, run `/subscribe server:<their server ID>`. Server IDs come from right-clicking the server icon with Developer Mode on and picking **Copy Server ID**. For a server on another instance, add its instance address: `/subscribe server:1122334455667788990@alerts.example.org`. `/setup` and `/subscriptions` both show your own server's address to hand out.
26. Their mods get a request card with **Approve**, **Approve and follow back**, and **Deny**. Approve is one-way: their alerts flow to you, yours do not flow to them. Follow back asks for yours in return, and you get a card to approve that.

### Setup B and C only: add peer instances

27. For each instance you trust, run `/instance peer add <their address>`, like `/instance peer add alerts.example.org`.
27.5. Send that operator your own address from `/instance status` (a hostname for setup C, or a `key-...` address for setup B), so they can add you back. In `allowlist` mode nothing gets through until both sides have added each other.

The instance clocks have to agree within 5 minutes, since messages older than that are dropped to stop replays. Any machine running NTP is fine.

### Updating

Nothing in the build is pinned to a version, so updating is the same three commands every time: `git pull`, then `docker compose build --pull`, then `docker compose up -d`. Skip `--pull` and Docker quietly reuses whatever Go and base images it downloaded the first time, which is how "always latest" turns into "months old". Once a month is a fine rhythm, and right away when a Go security release comes out.

### Reverse proxy in Docker

If your reverse proxy is a container too (NPM, Caddy, Traefik, or whichever), `127.0.0.1` inside the proxy container is the proxy itself, not your machine, so the `ports:` line will not reach it. Put both containers on the same Docker network instead and point the proxy at `http://gossipwire:8081`. With NPM, for example, that means adding something like this to `compose.yml`, using the network name your proxy is on:

```yaml
    networks:
      - proxy

networks:
  proxy:
    external: true
```

The first three lines go under `gossipwire:`, next to `volumes:`. The last block goes at the very bottom of the file.

### Portainer

In Portainer, add the stack with the **Repository** option pointed at the repo, which clones and builds it for you. Use absolute paths for the volume and the token file (`/opt/gossipwire/data`, `/opt/gossipwire/discord_token.txt`, or wherever you keep them), since relative paths resolve inside Portainer's own stack folder.

If Portainer's file secrets give you trouble, you can delete the `DISCORD_TOKEN_FILE` line and both `secrets:` blocks and set `DISCORD_TOKEN: "your token"` in `environment:` instead. That works, but the token then shows up in `docker inspect` and in Portainer's stack view, so anyone who can see either can take over the bot. Setting both is a startup error.

## FEDERATION_MODE in plain words

This setting decides who your servers can trade alerts with. It is the most important line in the compose file, so the bot refuses to start without it.

- **`off`**: your bot only talks to servers that use your bot. No other instance can reach you, and you cannot reach them. The safest choice, and the right one if you are not sure.
- **`allowlist`**: same as `off`, plus the instances you personally add with `/instance peer add`. Nobody else gets in.
- **`open`**: any GossipWire instance on the internet can send your servers a subscribe request. Needs `INSTANCE_URL`, since outsiders have to be able to reach you.

In every mode, a server's own mods still approve every subscription. The mode only decides who is allowed to ask. `open` does not mean anyone can push alerts into your servers. It means anyone can knock.

Federating with an instance means trusting its operator. They hold that bot's token and database, so they could forge an alert from any server on their instance. Every alert card shows which instance it came through, so you always know who you are trusting.

## Settings reference

| Setting | Required | Default | What it does |
| --- | --- | --- | --- |
| `OWNER_ID` | Yes | none | Your Discord user ID. The only person who can run `/instance`, and who gets DMs about key changes and held alerts. |
| `FEDERATION_MODE` | Yes | none | `off`, `allowlist` or `open`. Who your servers can trade alerts with. See FEDERATION_MODE in plain words. |
| `INSTANCE_URL` | Setup C only | empty | Your public https address. Empty means other instances cannot reach you directly. Never change it once peers know you. |
| `DISCORD_TOKEN_FILE` | One of these two | none | Path inside the container to a file holding the bot token. The safer option. |
| `DISCORD_TOKEN` | One of these two | none | The bot token itself. Easier in Portainer, visible to anyone who can inspect the container. |
| `FED_LISTEN` | No | `0.0.0.0:8081` | Address the federation endpoint listens on inside the container. Only read when `INSTANCE_URL` is set. Almost nobody needs to change it. |
| `LOG_LEVEL` | No | `info` | `debug`, `info`, `warn` or `error`. Set `debug` when something is not working and you want to see why. |
| `DATA_DIR` | No | `/data` | Only for running the binary outside the container. Leave it alone in Docker. |

Day-to-day tuning lives in Discord, under `/instance`, so nothing is set in two places:

| Command | What it does |
| --- | --- |
| `/instance status` | The setup this instance runs as, federation mode, public address, peers, limits, server count and the install link. The first thing to run when something seems off. |
| `/instance peer add` / `peer remove` | Allows or cuts off a peer instance. Remove is immediate and drops every subscription with that instance's servers. |
| `/instance peers` | Lists peers with their status and key fingerprint. A peer whose key changed shows **Approve key** and **Keep blocked** buttons. |
| `/instance limits` | Shows or changes the rate limits (default 5 alerts per hour and 20 per day, per server, forwards included) and the evidence cap (default 8 MB). |

Only the `OWNER_ID` user can run `/instance`, from any server the bot is in where you hold Manage Server (the command is hidden from everyone else), and the replies are only visible to you.

Everything else is for each server's own mods, and needs both **Manage Server** and **Ban Members** in that server:

| Command | What it does |
| --- | --- |
| `/setup channel:` | Sets the alert channel. Running it again moves the channel. |
| `/pause` / `/resume` | Stops or restarts incoming alerts without touching subscriptions. |
| `/alert userid: reason:` | Sends an alert to every server that follows yours. Optional `evidence` attachment and `link` to the message. Only works in the alert channel. You get a confirm card with the username and avatar first. Evidence has to be an image (PNG, JPEG, GIF, WebP), a video (MP4, WebM) or a plain text file, since the bot re-posts it in other servers. The type is read from the file itself, not its name. |
| `/retract alert_id:` | Marks an alert your server sent as retracted, on every copy, forwards included. The ID is in the card's footer. Bans already made stay. |
| `/subscribe server:` | Asks to receive a server's alerts. |
| `/unsubscribe server:` | Stops receiving. Immediate. |
| `/subscribers remove server:` | Stops sending to a server that follows you. Immediate. |
| `/subscriptions` | Who you follow, who follows you, and pending requests both ways. |
| `/history` | Alerts sent and received in the last 14 days, or about one user with `userid:`. |

Alert cards carry three buttons. **Ban** bans by user ID, which works even if they have not joined yet. **Ignore** closes the card without banning. **Forward** passes the alert to the servers that follow yours, with your server added to the chain on the card. Each server gets a given alert once, and forwarding ends 14 days after the alert was first sent. After 14 days the bot forgets the alert; the cards in Discord keep the record.

## Troubleshooting

Every startup problem prints one line starting with `GossipWire cannot start:` and the container stops. Run `docker compose logs gossipwire` to see it.

| Message | Cause | Fix |
| --- | --- | --- |
| `OWNER_ID is missing.` | The `OWNER_ID` line is empty or gone. | Put your Discord user ID there (step 15). |
| `OWNER_ID is still the example value 123456789012345678.` | The example was left in. | Replace it with your own ID. |
| `OWNER_ID "zack" is not a Discord user ID.` | A username or something else went in. | Use the long number from **Copy User ID**. |
| `FEDERATION_MODE is missing.` | The line is empty or gone. | Set it to `off`, `allowlist` or `open`. If unsure, `off`. |
| `FEDERATION_MODE "..." is not a valid choice.` | A typo. | Use exactly `off`, `allowlist` or `open`. |
| `FEDERATION_MODE is open, but INSTANCE_URL is empty.` | Open mode needs a public address. | Set `INSTANCE_URL`, or use `allowlist`. |
| `INSTANCE_URL must start with https://.` | Plain http, or no scheme. | Use `https://your.domain`. Your reverse proxy provides the https. |
| `INSTANCE_URL "..." is not a web address.` | Something that is not a URL. | Use the form `https://alerts.yourdomain.net`. |
| `INSTANCE_URL "..." has extra parts after the address.` | A path like `/bot` was added. | Use only the scheme and host. GossipWire needs its own hostname or subdomain. |
| `INSTANCE_URL is still an example address` | `alerts.example.org` was left in. | Put your own address there, or `""` for setup A or B. |
| `Both DISCORD_TOKEN_FILE and DISCORD_TOKEN are set.` | Both token methods are in the file. | Keep one. See the Portainer section. |
| `No bot token found.` | `discord_token.txt` is missing, empty, or Docker made a folder by that name because the file did not exist. | Delete any `discord_token.txt` folder, then redo step 11. |
| `The bot cannot read discord_token.txt.` | The file belongs to your user and the container runs as UID 65532. | `sudo chown 65532:65532 discord_token.txt` (step 13). |
| `The bot token has spaces or more than one line in it.` | Extra text got pasted into the file. | The file should hold only the token, on one line. Redo step 11. |
| `LOG_LEVEL "..." is not a valid choice.` | A typo. | Use `debug`, `info`, `warn` or `error`, or remove the line. |
| `Cannot write to /data.` | The data folder belongs to your user, not the container's. | `sudo chown 65532:65532 ./data`, or add `:U` to the volume line on rootless Podman. |
| `/data/identity.key is damaged.` | The key file was edited or half-copied. | Restore it from your backup. Deleting it gives the instance a new identity, which peers treat as a stranger. |
| `could not set up the database` | The database file is damaged or the disk is full. | Free up space, or restore `data/` from your backup. |
| `Discord rejected the bot token.` | The token was reset in the portal, or copied wrong. | Get a new one under Bot > Reset Token and redo step 11. |
| `Could not reach Discord to check the bot token` | No internet or DNS on the machine. | Check the connection; the bot only needs outbound https. |
| `Could not register slash commands with Discord` | Usually a temporary Discord outage. | Restart the container in a few minutes. |
| `Cannot listen on FED_LISTEN 0.0.0.0:8081` | Something else in the container is on that port, or `FED_LISTEN` was changed to a bad value. | Remove the `FED_LISTEN` line to use the default. |
| `Could not connect to Discord` | The gateway connection failed. | Check the connection and restart the container. |

Two warnings print once and do not stop the bot: `INSTANCE_URL is set but FEDERATION_MODE is off, so it is ignored.` and `INSTANCE_URL ended with a slash. I removed it.` Fix the line when you get a chance.

Problems after startup:

- **The commands do not show up in Discord.** Global commands can take a few minutes to appear the first time. Also check that you have Manage Server and Ban Members, since the commands are hidden from everyone else.
- **Ban says my role needs to be higher.** Drag the GossipWire role above the target's highest role in Server Settings > Roles (step 22).
- **A card says "not verified".** "Origin not verified" means this instance could not check the sending instance's signature, usually because that instance has no public address and you have never peered with it. "(not verified)" next to a forward means the same for that hop. The instance you peer with vouches for those parts, so trust them as far as you trust its operator.
- **A peer's alerts stopped arriving.** Run `/instance peers`. If the peer shows `key_changed`, its key no longer matches the one pinned here. That happens when an operator wipes their data folder, and also when a domain gets hijacked. Ask the operator out of band before you press **Approve key**.
- **Federation messages are refused as too old.** The two machines' clocks are more than 5 minutes apart. Turn on NTP on both.
- **Anything else.** Set `LOG_LEVEL: "debug"`, run `docker compose up -d`, and read `docker compose logs -f gossipwire`.

## Where things stand

Now GossipWire is online in your server, `/setup` has pointed it at your mod channel, and `/instance status` shows the setup you picked. Alerts from servers you follow land in that channel with a Ban button, and your own `/alert` goes out to every server that follows you. Tinker with the limits under `/instance limits` as you see fit.
