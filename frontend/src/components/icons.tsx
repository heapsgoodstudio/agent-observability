// The bespoke 16px icon set from the Figma library (02 Components ·
// Navigation). Every glyph draws in currentColor so nav items and controls
// recolor them through text classes; all are decorative (aria-hidden), the
// accessible name lives on the surrounding link or control.
export interface IconProps {
  size?: number;
}

function Svg({ size = 16, children }: IconProps & { children: React.ReactNode }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" aria-hidden="true" className="shrink-0">
      {children}
    </svg>
  );
}

export function IconOverview({ size }: IconProps) {
  return (
    <Svg size={size}>
      {[
        [1.5, 1.5],
        [9, 1.5],
        [1.5, 9],
        [9, 9],
      ].map(([x, y]) => (
        <rect key={`${x}-${y}`} x={x} y={y} width={5.5} height={5.5} rx={1.8} fill="currentColor" />
      ))}
    </Svg>
  );
}

export function IconSpend({ size }: IconProps) {
  return (
    <Svg size={size}>
      <rect x={2.5} y={8} width={3} height={6} rx={1.2} fill="currentColor" />
      <rect x={6.5} y={3} width={3} height={11} rx={1.2} fill="currentColor" />
      <rect x={10.5} y={5.5} width={3} height={8.5} rx={1.2} fill="currentColor" />
    </Svg>
  );
}

export function IconSessions({ size }: IconProps) {
  return (
    <Svg size={size}>
      {[3, 7, 11].map((y) => (
        <g key={y}>
          <rect x={2} y={y} width={2} height={2} rx={1} fill="currentColor" />
          <rect x={6} y={y} width={8} height={2} rx={1} fill="currentColor" />
        </g>
      ))}
    </Svg>
  );
}

export function IconInsights({ size }: IconProps) {
  return (
    <Svg size={size}>
      <path
        d="M 8 1 L 9.8 6.2 L 15 8 L 9.8 9.8 L 8 15 L 6.2 9.8 L 1 8 L 6.2 6.2 Z"
        fill="currentColor"
      />
    </Svg>
  );
}

export function IconHealth({ size }: IconProps) {
  return (
    <Svg size={size}>
      <path
        d="M 1.5 8.5 H 4.5 L 6.5 3.5 L 9.5 12.5 L 11.5 8.5 H 14.5"
        fill="none"
        stroke="currentColor"
        strokeWidth={1.6}
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </Svg>
  );
}

export function IconSearch({ size }: IconProps) {
  return (
    <Svg size={size}>
      <circle cx={6.25} cy={6.25} r={4.25} fill="none" stroke="currentColor" strokeWidth={1.5} />
      <path d="M 10.8 10.8 L 14 14" stroke="currentColor" strokeWidth={1.5} strokeLinecap="round" />
    </Svg>
  );
}

export function IconClose({ size }: IconProps) {
  return (
    <Svg size={size}>
      <path
        d="M 4.5 4.5 L 11.5 11.5 M 11.5 4.5 L 4.5 11.5"
        stroke="currentColor"
        strokeWidth={1.5}
        strokeLinecap="round"
      />
    </Svg>
  );
}

export function IconChevronDown({ size }: IconProps) {
  return (
    <Svg size={size}>
      <path
        d="M 4.5 6.5 L 8 10 L 11.5 6.5"
        fill="none"
        stroke="currentColor"
        strokeWidth={1.5}
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </Svg>
  );
}
