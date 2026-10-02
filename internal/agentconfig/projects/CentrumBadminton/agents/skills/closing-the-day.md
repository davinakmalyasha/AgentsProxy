# Closing the day

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

5. **Report the three numbers** — court, cantein, expenses — and the day's
   net. Keep it to three or four sentences.

Never invent a figure. If a step cannot be completed, say which one and why.