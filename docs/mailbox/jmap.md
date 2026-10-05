---
title: JMAP mailboxes
description: Use JMAP instead of IMAP and SMTP for a mailbox, and tune how Pelton syncs it.
---

# JMAP mailboxes

JMAP is a newer mail protocol that runs over HTTPS. One connection does the
work of both IMAP and SMTP: it lists folders and messages, fetches mail,
pushes new-mail notices, and sends. Servers such as Stalwart and Fastmail
offer it next to IMAP. Pelton never makes you use it. IMAP stays the default.

## Checklist
<div class="checklist" markdown>
- [ ] Add the mailbox and run the connection test
- [ ] Choose JMAP when Pelton offers it
- [ ] Check the connection line in Settings
</div>

## When Pelton offers JMAP

Pelton offers JMAP only when the server advertises both mail and mail
submission (sending) for your account. After a successful **Test
connection** in the add-mailbox wizard, Pelton looks for JMAP on the same
server and, if it is there, asks **Use JMAP for mail and send?** Pick
**Use JMAP** or **Keep IMAP**. If you do nothing special, the mailbox stays
on IMAP.

The wizard is the same as for [any IMAP/SMTP account](imap-smtp.md). The
incoming server field is labeled **IMAP/JMAP host**.

- [x] Add the mailbox and run the connection test
- [x] Choose JMAP when Pelton offers it

## What changes on JMAP

For a JMAP mailbox, Pelton uses JMAP for:

- **Sync.** Folders, message lists and message bodies all come over JMAP.
- **Live updates.** New mail arrives through a WebSocket push connection.
- **Sending.** Messages go out over JMAP. SMTP is not used for that mailbox.

Your IMAP and SMTP settings stay saved, so you can switch back at any time.

Settings > Accounts shows a connection line under each mailbox:
`IMAP · host:port` or `JMAP · host`.

## Switching later

Open **Settings > Accounts**, edit the mailbox, and turn **Use JMAP for mail
and send** on or off. If the server does not support JMAP, the toggle is disabled and says
"JMAP is not available on this server". Switching back to IMAP is always
allowed.

Switching clears that mailbox's cached mail and downloads it again. Other
mailboxes, address books and the outbox are not touched. On a large mailbox
the download takes a while, and the message list fills in as it goes.

## Contacts

Contacts stay on CardDAV. For a password mailbox on JMAP, Pelton finds the
address books itself, using `/.well-known/carddav` on your email domain, and
adds them. OAuth mailboxes add their address books by hand.

## Proxies and certificates

A mailbox's proxy route and trusted certificates (including a custom CA)
apply to every JMAP connection too, and to the CardDAV auto-setup. **Test
route** covers the JMAP host. With a proxy on, Pelton skips DNS SRV lookups.

If the JMAP server presents a certificate Pelton does not trust, you get the
same review prompt as for IMAP: in the wizard, in the mailbox editor before
the switch happens, and in the sync failure dialog. Check the fingerprint,
then choose **Trust this certificate**.

## Syncing

Pelton lists message stubs (sender, subject, date) first. In a large mailbox
the list appears page by page. Message bodies are then downloaded for the
newest messages in each folder. Two settings in **Settings > Sync & power**
shape this:

- **Messages to sync per folder**: how many of the newest bodies to
  download per folder. The default is 100. **All** downloads every body,
  newest first. Older mail loads as you scroll, and opening a message that
  has not been downloaded yet fetches it.
- **Parallel sync connections**: how many connections one mailbox may use
  for syncing, from 1 to 5. The default is 3. Each mailbox can override it
  in its editor: turn off **Parallel sync connections: use default (n)**, where n is
  the current global value, and set its own number. Sending never waits for these connections, and the JMAP
  push socket does not count against them.

Both settings apply to IMAP mailboxes as well.

## Not supported

- Calendars
- JMAP contacts (use CardDAV)
- Sieve and vacation responses
- Server-side drafts. Drafts stay on your computer.

Search uses Pelton's local index, not a server-side JMAP search.

## Backups

Backups include the mailbox protocol and its parallel connections override.
A JMAP mailbox is restored as JMAP. Restored without its password, it waits
for one: enter it when Pelton asks, or in Settings > Accounts, and the mailbox
syncs straight away, finding the JMAP server as it connects. If the server cannot be reached then,
the sync fails and the following sync tries again; the mailbox stays on JMAP.

## Troubleshooting

??? question "Pelton never asks about JMAP"
    The server has to advertise both mail and submission for your account.
    Some servers offer only one, or JMAP is off for your account. Ask your
    provider or administrator, or stay on IMAP.

??? question "The server certificate is not trusted"
    Pelton shows the certificate review prompt. Compare the fingerprint with
    the one your server shows and choose **Trust this certificate**, or add
    your own certificate authority in the mailbox's certificate settings.

??? question "Switching takes a long time"
    The mailbox's cache is cleared and downloaded again. On a large mailbox
    that takes a while. Raise **Parallel sync connections** or lower
    **Messages to sync per folder** to speed up the bodies.

## Need help?

Still stuck? See [Support](../support.md).
