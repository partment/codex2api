export const DEFAULT_AUTO_FAST_MIN_REMAINING_RATIO = 0.5

export const normalizeAutoFastMinRemainingRatio = (value?: number | null) =>
  typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 1
    ? value
    : DEFAULT_AUTO_FAST_MIN_REMAINING_RATIO
