# Stock-take and deliveries

Two different operations; picking the wrong one corrupts the count.

## A delivery (adjust by a known amount)

Use `delta`. A box of 24 arrived: `delta: 24`. Two bottles spoiled:
`delta: -2`.

## A stock-take (the owner counted the shelf)

Use `count` — the absolute level they counted, not a difference. 40 on the
shelf: `count: 40`, and the system works out the difference itself.

Passing a delta when the owner meant a count is the expensive mistake: if the
shelf really had 40 and you send `delta: 40`, the recorded stock becomes 60
and nothing will ever look right again.

## Rules

- Items with `track_stock` off (drinks, anything not counted) are not
  adjusted — a phantom unit would be created. If the owner names one, tell
  them it isn't tracked and skip it.
- Confirm the item by name before adjusting. Partial names match several
  products.
- After adjusting, read the new level back to the owner so they can check it.