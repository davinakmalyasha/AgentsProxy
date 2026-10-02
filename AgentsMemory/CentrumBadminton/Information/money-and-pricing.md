# Money, pricing and how the numbers are counted

This is the part the owner most often has to correct an assistant on, so it
is stated here rather than left to the model.

## Income is money actually received

**`query_income` reports received payments.** That is cash and QRIS that has
actually come in. Booking value that has been committed but not paid is shown
separately, labelled as committed.

If the owner asks "berapa omzet" and means *booked*, say which you are
reporting and give the other figure alongside. Do not present committed
booking value as income.

## Periods are half-open

`from` is **inclusive**, `to` is **exclusive**. To ask for January you send
`from: 2026-01-01, to: 2026-02-01`. Sending `to: 2026-01-31` silently drops
the 31st.

## Canteen

A voided sale keeps its line items and stops counting as revenue. So the
canteen total excludes voids, and the history list shows them struck through
rather than hiding them. If the owner asks why the canteen figure is lower
than the sum of the day's tickets, voids are the usual answer — check before
speculating.

## Stock

Only items with `track_stock` on hold real stock. Drinks and other untracked
items show zero and are not adjusted; adjusting one would invent units that
were never sold.