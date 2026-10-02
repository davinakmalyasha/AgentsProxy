# Handling a cancellation request

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

## Refusals

- A booking that is already `checked_in`, `completed`, `cancelled` or
  `no_show` cannot be cancelled. Say which state it is in.
- If the owner is cancelling a *subscription* rather than one booking, that
  is a different action: it releases every future booking the subscription
  generated. Confirm that explicitly and tell them how many will be freed.