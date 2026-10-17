# `pkg/idgen`: IDs and short codes

**What it does.** Generates unique 64-bit IDs without asking the database, and
writes them in Base62 to make short codes. It lives in `pkg/` because it has no
dependency on the rest of the project and could be reused anywhere.

**Where it sits.** `cmd/server` creates one `Generator` and gives it to both
services. Link and user IDs come from it. Short codes are the Base62 form of
the link ID, unless the user chose an alias.

## Snowflake IDs (`idgen.go`)

An ID packs three numbers into one `int64`:

```
 41 bits: milliseconds since 2024-01-01 UTC │ 10 bits: node ID │ 12 bits: sequence
```

- **Time** comes first, so IDs sort by creation time. Listing links "by ID"
  is therefore listing them oldest first.
- **Node ID** (`NODE_ID`, 0–1023) separates server instances. Two instances
  with the **same** node ID can produce the same ID in the same millisecond.
- **Sequence** counts up to 4096 IDs within one millisecond on one node. If a
  node needs more, `NextID` waits for the next millisecond.

41 bits of milliseconds last about 69 years from the 2024 epoch.

`Generator` is safe to use from many goroutines (a mutex guards it).
`NewGenerator` rejects node IDs outside 0–1023.

## Base62 (`base62.go`)

`ToBase62(n)` writes `n` with the 62 characters `0-9A-Za-z`. A Snowflake ID
from this epoch gives a **10-character** code today, and 11 characters from
about 2030 onward.

**Why not random codes?** Random codes need a "does this already exist?" check
and a retry loop on every creation. An ID that is unique by construction gives
a unique code by construction. The only exception is a collision with a
user-chosen alias, which the link service handles by drawing a new ID (see
[service.md](service.md#creating-a-link-shorten)).

**A side effect:** codes are predictable. Consecutive links get similar codes,
and anyone can estimate how many links were created. That is acceptable for a
public URL shortener, where a short code is not a secret.

## Before you change it

- **Never change the epoch or the bit layout** on a running system. New IDs
  could repeat old ones.
- Tests in `idgen_test.go` cover uniqueness under concurrency, ordering, the
  node ID bounds and Base62 encoding.
