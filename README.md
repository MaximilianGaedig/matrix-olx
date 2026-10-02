# mautrix-olx

A Matrix–OLX puppeting bridge: your [OLX](https://www.olx.pl) chats as Matrix rooms. Built on
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

## Logging in

Send `login` to the bridge bot. It answers with a link to OLX's own login page. Open it, log in if
OLX asks, and you end up on an address starting with `https://www.olx.pl/d/callback/?code=…`. Paste
that address back to the bot.

The bridge never sees your password and takes nothing from your browser: it gets a session of its
own, the same way OLX's website does (OAuth authorization code with PKCE), and keeps it alive with
its refresh token.

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
