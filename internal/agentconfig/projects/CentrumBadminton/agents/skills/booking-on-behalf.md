# Taking a booking by message

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