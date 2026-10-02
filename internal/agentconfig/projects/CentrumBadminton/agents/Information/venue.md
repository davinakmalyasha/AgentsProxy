# Opening hours and courts

## Courts

The venue runs badminton courts, referred to by name. Resolve a name to its
id with `get_availability` — never guess or construct an id.

Court names currently in use: **Lapangan Satu**, **Lapangan Dua**. The list
changes; ask `get_availability` rather than assuming this list is current.

## Hours

The venue is open **07:00–23:00 WIB**, every day. A booking outside those
hours is refused, so if the owner asks for one, say the hour is out of range
and offer the nearest slot inside it.

## Booking horizon

Bookings are accepted for **today and future dates**. There is no limit on
how far ahead, but a booking more than a few months out is almost always a
mistake — confirm the date if it looks unusual.

## Today

All dates are **WIB (Asia/Jakarta, UTC+7)**. A day boundary in UTC would be
wrong: between 07:00 and 14:00 WIB the UTC date is already tomorrow. Every
figure the agent reports is filtered on the WIB day, so "today" always means
the same thing to the owner and to the data.