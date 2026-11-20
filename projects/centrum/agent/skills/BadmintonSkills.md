# Badminton Skills — Centrum Badminton Arena

How things are done here. These are procedures, not facts: facts about the
venue come from the database through tools, and tool definitions live in the
platform registry. Each section below names the tools it uses; if a tool it names is not available
to your role, say so instead of improvising around it.

---

## Closing the day

Work in this order. Do not skip a step because the owner is in a hurry — a
closing report with an unrecorded cash payment in it is worse than a slow one.

1. **Settle the day first.** Call `closing_report` for the date. It returns
   court revenue, canteen takings and expenses for that day.

2. **Check for unpaid bookings before declaring the day closed.** If the
   report mentions bookings still unpaid, list them and ask the owner whether
   they were paid in cash outside the system. Do not assume.

3. **Record any payment the owner confirms.** Use `record_staff_payment` only
   for payments made *to staff*. A customer paying for a booking is a
   booking payment, not a staff payment — if you are unsure which one is
   meant, ask.

4. **Void anything rung up by mistake.** A void keeps the line items and stops
   the sale counting as revenue. Say how many you voided.

5. **Report the three numbers** — court, canteen, expenses — and the day's
   net. Keep it to three or four sentences.

Never invent a figure. If a step cannot be completed, say which one and why.

---

## Handling a cancellation request

Cancelling releases a court. It is reversible in the record but not in fact —
a re-booked slot cannot be un-taken — so confirm before acting.

1. **Identify the booking.** The owner usually names a court and a time.
   Call `list_bookings` for that date and match it. If two bookings could
   match, ask which rather than guessing.

2. **State what will happen before doing it.** "Batalkan booking Pak Budi,
   Lapangan Dua, 19:00–20:00 tanggal 3 Oktober?" — then wait.

3. **Only then call `cancel_booking`.**

4. **Report what actually changed.** A cancellation keeps the row with
   status `cancelled`; it is not deleted, and the money is still recorded.

### Refusals

- A booking that is already `checked_in`, `completed`, `cancelled` or
  `no_show` cannot be cancelled. Say which state it is in.
- If the owner is cancelling a *subscription* rather than one booking, that
  is a different action: it releases every future booking the subscription
  generated. Confirm that explicitly and tell them how many will be freed.

---

## Taking a booking by message

1. **Resolve the court.** The owner says "Lapangan Dua", not a uuid. Call
   `get_availability` for the date and find the court by name.

2. **Check the slot is actually free** in the same response. If it is
   blocked or taken, say so and offer the nearest free slot — do not book
   optimistically and apologise afterwards.

3. **Confirm the details** before writing: court, date, start, end, and who
   the booking is for. If the owner gave all of them, do not ask again — just
   book and report what you booked.

4. **The price is not yours to set.** It is computed from the pricing rule
   for that time of day. If the owner quotes a different number, book it and
   mention the difference; an override is the owner's call, not yours.

5. **Report the confirmation** — court, date, time — and nothing more.

A phone number is optional. If one is given, store it; it is what the owner
uses to reach the player later.

---

## Stock-take and deliveries

Two different operations; picking the wrong one corrupts the count.

### A delivery (adjust by a known amount)

Use `delta`. A box of 24 arrived: `delta: 24`. Two bottles spoiled:
`delta: -2`.

### A stock-take (the owner counted the shelf)

Use `count` — the absolute level they counted, not a difference. 40 on the
shelf: `count: 40`, and the system works out the difference itself.

Passing a delta when the owner meant a count is the expensive mistake: if the
shelf really had 40 and you send `delta: 40`, the recorded stock becomes 60
and nothing will ever look right again.

### Rules

- Items with `track_stock` off (drinks, anything not counted) are not
  adjusted — a phantom unit would be created. If the owner names one, tell
  them it isn't tracked and skip it.
- Confirm the item by name before adjusting. Partial names match several
  products.
- After adjusting, read the new level back to the owner so they can check it.
