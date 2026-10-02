# mautrix-olx

A Matrix–OLX puppeting bridge: your OLX chats (olx.pl, olx.ua, olx.ro, olx.bg, olx.pt, olx.kz, olx.uz) as Matrix rooms. Built on
[mautrix-go](https://github.com/MaximilianGaedig/mautrix-go)'s bridgev2, like the other mautrix bridges.

OLX has no public chat API. This bridge speaks the one its website uses: a REST API on
`api.chat.olx.pl`, a WebSocket for live events, and the public profile API for names, pictures and
online status.

## What is bridged

An OLX chat is always between you and one other person, about one ad. Each chat is a direct-chat
room named after the person and the ad, with the ad's photo as its avatar and the ad's price,
place, state and link in the topic.

| | OLX → Matrix | Matrix → OLX |
|---|---|---|
| Text | ✓ | ✓ |
| Photos | ✓ | ✓ (JPEG/PNG as is, other formats re-encoded as JPEG) |
| Documents (PDF, Word, Excel) | ✓ | ✓ |
| Captions | ✓ | ✓ |
| System messages and widgets (price proposals, questions) | ✓ as notices | – |
| Typing notifications | ✓ | ✓ |
| Read receipts | ✓ | ✓ |
| Presence (online / last seen) | ✓ | – |
| Message history (backfill) | ✓ | |
| Trash ("Kosz") as a room tag | ✓ | ✓ |
| Saved list ("Zapisane") as a room tag | ✓ | ✓ |
| Blocking (Matrix ignore list) | ✓ | ✓ |
| Deleting a chat | – | ✓ (to the trash, or for good, by config) |
| Starting a chat about an ad | | ✓ (`message-ad`) |
| Own messages from other devices (double puppeting) | ✓ | |
| Delivery status | | ✓ |

OLX's chat has no replies, edits, reactions, message deletion, voice messages, videos or calls, so
there is nothing to bridge there; Matrix clients are told so through the room's capabilities.

## Sites

OLX runs one site per country on the same platform, and an account belongs to one of them. The
bridge works with all of them: olx.pl, olx.ua, olx.ro, olx.bg, olx.pt, olx.kz and olx.uz. Each
login is for one site; `network.default_site` only decides which one is offered first.

## Logging in

Send `login` to the bridge bot and pick a method for your site.

**Login page** (`page-pl`, `page-ua`, …) gives the bridge a session of its own, the way OLX's
website gets one (OAuth authorization code with PKCE). The bridge never sees your password. OLX's
login ends on a page that immediately jumps to the home page, so the address it ends on has to be
read without letting that page run: copy the link the bot gives you, type `view-source:` into the
address bar of a new tab, paste the link after it and press Enter. The address bar then shows
`view-source:https://www.olx.pl/d/callback/?code=…`; send that to the bot. (If you opened the link
normally, the same address is in the browser's history.)

**Session token** (`token-pl`, …) copies the session of a browser that is logged in: the bot gives
you one line to paste into that browser's developer console, which puts the token on the clipboard.
The bridge then shares that browser's session.

## Presence

OLX's chat has no presence, but ads show whether the seller is online and when they were last
seen. The bridge reads that from the profile API for the people in your most recent chats, and
counts anyone who writes, types or reads as online for a few minutes.

OLX refuses requests to `www.olx.pl` from datacenter addresses. On a hosted server, profile
pictures and polled presence therefore need `network.www_proxy` to point at a proxy on a connection
OLX accepts (a home connection). Chatting does not depend on it.

## Commands

Besides the standard bridge commands (`login`, `logout`, `sync-chats`, `delete-portal`, …):

- `message-ad <ad ID or address> <message>` – write to the seller of an ad. This starts a chat
  about the ad, or continues the one you already have.
- `start-chat <ad ID or address>` – open the room of the chat you have about an ad.

## Building

```sh
./build.sh
```

or `docker build .`. Tests: `go test -tags goolm ./...`.
