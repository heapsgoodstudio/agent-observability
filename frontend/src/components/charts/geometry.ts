// Pure SVG path arithmetic shared by chart components.

// Rect path with rounded top corners only: the data-end of a column gets the
// radius, the baseline (and stack interior) stays square.
export function topRoundedRect(xPos: number, yPos: number, w: number, h: number, r: number): string {
  const rr = Math.min(r, w / 2, h);
  return `M ${xPos} ${yPos + h} L ${xPos} ${yPos + rr} Q ${xPos} ${yPos} ${xPos + rr} ${yPos}`
    + ` L ${xPos + w - rr} ${yPos} Q ${xPos + w} ${yPos} ${xPos + w} ${yPos + rr}`
    + ` L ${xPos + w} ${yPos + h} Z`;
}
