# search-hello — lexical search via `txco://search/*`

A stack that indexes a few handbook pages and finds them again by the words
they contain. No external service and nothing to install: the search store is
a chassis built-in (Bleve, on local disk).

```
OPS/search-demo/
  100/ensure.txcl     /seed    EXEC "txco://search/collection"
  150/seed.txcl       /seed    EXEC "txco://search/upsert"   (four records)
  100/find.txcl       /find    EXEC "txco://search/query"    filter: audience = anyone
  100/find_team.txcl  /find    …&as=team                     filter: audience in [anyone, team]
  100/rename.txcl     /rename  EXEC "txco://search/update"   merge + fields, text untouched
  100/forget.txcl     /forget  EXEC "txco://search/delete"   by filter
  200/*.txcl          serialize the result (or the structured error)
```

Run it:

```
txco dev          # from this directory
curl 'http://localhost:8080/seed'
curl 'http://localhost:8080/find?q=where+can+I+leave+my+luggage'
curl 'http://localhost:8080/find?q=TXC-4821'            # nothing: the ticket is the team's
curl 'http://localhost:8080/find?q=TXC-4821&as=team'    # the ticket
curl 'http://localhost:8080/find?q=%22termination+for+convenience%22&as=team'
curl 'http://localhost:8080/rename'
curl 'http://localhost:8080/find?q=garage.md'
curl 'http://localhost:8080/forget?doc=contract.md'
```

Things to notice:

- **A whole sentence is a fine query.** Words are OR-ed and ranked, so the
  luggage question finds its page without any keyword picking.
- **`TXC-4821` is matched whole.** It finds the ticket ahead of anything that
  merely contains `TXC` or `4821`.
- **The quoted phrase ranks the contract first**, although the ticket holds the
  same three words scattered through one sentence.
- **The filter admits or rejects; it never ranks.** It is the same grammar
  `txco://vector/search` takes, so one filter object can serve both lanes.
- **`/rename` changes a filename without re-indexing any text**, and the new
  name finds the page at once.
- **Errors are data.** Start the chassis with `--search-store=none` and every
  route answers `{"error":"txco_search_disabled"}`, from the rule in
  `200/error.txcl`. The ops never go missing.

The index lives under `.txco/dev/search/`. Delete it and `/seed` again: a search
index is a projection of records the stack can produce again.

More: [`docs/search.md`](../../docs/search.md).
