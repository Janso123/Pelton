-- sent_count is how many messages the user has sent to an address. It ranks
-- the address book ahead of use_count, which also counts received mail: the
-- people the user writes to come first and are evicted last, however often a
-- newsletter writes to them. use_count stays as the overall tally the
-- settings manager shows.
ALTER TABLE address_book ADD COLUMN sent_count INTEGER NOT NULL DEFAULT 0;

-- Until now use_count summed the sends to an address and, for an entry the
-- harvest created, the mail received from it. Once, here, the sends are
-- estimated as use_count less the cached mail from that address (by its
-- first From address), so the people the user wrote to stay suggested at the
-- default level when their Sent copy is not cached. Keys an old harvest
-- stored as "name <addr>" only ever came from received mail and keep 0, and
-- so do bulk senders (the automated local parts storage.bulkLocalParts lists,
-- or cached mail carrying List-Unsubscribe): the harvest counted their mail
-- once, and mail deleted since would leave a positive estimate behind.
WITH lowered AS (
    SELECT lower(trim(from_address)) AS f, list_unsubscribe != '' AS listed
    FROM messages WHERE from_address LIKE '%@%'
), senders AS (
    SELECT CASE
             WHEN instr(f, '<') > 0 AND instr(f, '>') > instr(f, '<')
               THEN substr(f, instr(f, '<') + 1, instr(f, '>') - instr(f, '<') - 1)
             ELSE f
           END AS email,
           COUNT(*) AS received,
           MAX(listed) AS listed
    FROM lowered
    GROUP BY 1
)
UPDATE address_book
SET sent_count = MAX(0, use_count - COALESCE(
    (SELECT received FROM senders WHERE senders.email = address_book.email), 0))
WHERE instr(email, '<') = 0 AND instr(email, ',') = 0
  AND substr(email, 1, instr(email, '@') - 1) NOT IN
      ('noreply', 'no-reply', 'donotreply', 'do-not-reply', 'mailer-daemon', 'postmaster')
  AND NOT EXISTS (SELECT 1 FROM senders WHERE senders.email = address_book.email AND senders.listed);

DROP INDEX idx_address_book_rank;
CREATE INDEX idx_address_book_rank ON address_book(sent_count DESC, last_used DESC);
