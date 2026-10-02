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
| Price proposals | ✓ with their state and an Accept button | ✓ (`offer`, `accept-offer`, or the button) |
| System messages and OLX's questions | ✓ as notices | – |
| Reporting someone to OLX | | ✓ (`report`) |
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
bridge works with all of them: olx.pl, olx.ua, olx.ro, olx.bg, olx.pt, olx.kz and olx.uz.
`network.sites` lists the ones offered for login (olx.pl by default).

## Logging in

Send `login` to the bridge bot. It gives you a link to OLX's own login and takes back either of:

- **The address that login ends on.** This gives the bridge a session of its own, the way OLX's
  website gets one (OAuth authorization code with PKCE); it never sees your password. That address
  belongs to a page that jumps to the home page at once, so read it without letting the page run:
  the bot gives you the link with `view-source:` in front; paste that into the address bar of a new
  tab and send the address the tab ends on (`view-source:https://www.olx.pl/d/callback/?code=…`).
  Phone browsers have no `view-source:`; there, open the plain link and copy the
  `www.olx.pl/d/callback/?code=…` entry from the browser's history.
- **The session token of a logged-in browser.** The bot gives you one line for that browser's
  console that copies it. The bridge then shares the browser's session.

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
- `offer <price>` – in a chat's room: propose a price for its ad, or answer the other side's
  proposal with another price. OLX's limits for the ad apply and are told when a price is outside them.
- `accept-offer` – in a chat's room: accept the proposal that is waiting. The Accept button under a
  proposal sends this for exactly that proposal.
- `report [reason] [description]` – in a chat's room: report the other person to OLX's moderators.
  Without a reason it lists the ones OLX offers. There is deliberately no button for this.
- `start-chat <ad ID or address>` – open the room of the chat you have about an ad.

## Building

```sh
./build.sh
```

or `docker build .`. Tests: `go test -tags goolm ./...`.
