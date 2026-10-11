const DIGITS = ['không', 'một', 'hai', 'ba', 'bốn', 'năm', 'sáu', 'bảy', 'tám', 'chín'];

function readThreeDigits(n: number, hasHigherGroup: boolean): string[] {
  const hundreds = Math.floor(n / 100);
  const tens = Math.floor((n % 100) / 10);
  const units = n % 10;
  const words: string[] = [];

  if (hundreds > 0 || hasHigherGroup) {
    words.push(DIGITS[hundreds], 'trăm');
  }

  if (tens > 1) {
    words.push(DIGITS[tens], 'mươi');
    switch (units) {
      case 1:
        words.push('mốt');
        break;
      case 4:
        words.push('tư');
        break;
      case 5:
        words.push('lăm');
        break;
      default:
        if (units > 0) {
          words.push(DIGITS[units]);
        }
        break;
    }
  } else if (tens === 1) {
    words.push('mười');
    if (units === 5) {
      words.push('lăm');
    } else if (units > 0) {
      words.push(DIGITS[units]);
    }
  } else if (tens === 0) {
    if (units > 0) {
      if (hundreds > 0 || hasHigherGroup) {
        words.push('linh', DIGITS[units]);
      } else {
        words.push(DIGITS[units]);
      }
    }
  }

  return words;
}

/**
 * Converts a VND amount (number, bigint or string) into natural spoken Vietnamese text.
 * Aligned with Go internal/voicecopy/money_vi.go algorithm.
 */
export function speakVnd(rawAmount: string | number | bigint): string {
  let str = typeof rawAmount === 'string' ? rawAmount.trim() : rawAmount.toString();
  str = str.replace(/[^\d]/g, '');

  if (!str || /^0+$/.test(str)) {
    return 'không đồng';
  }

  str = str.replace(/^0+/, '');

  // Split into 3-digit groups from right to left
  const groups: number[] = [];
  while (str.length > 0) {
    const chunk = str.length > 3 ? str.slice(-3) : str;
    str = str.length > 3 ? str.slice(0, -3) : '';
    groups.push(parseInt(chunk, 10));
  }

  while (groups.length % 3 !== 0) {
    groups.push(0);
  }

  const numBlocks = groups.length / 3;
  const resultParts: string[] = [];
  let hasHigherOverall = false;

  for (let b = numBlocks - 1; b >= 0; b--) {
    const g2 = groups[b * 3 + 2]; // triệu
    const g1 = groups[b * 3 + 1]; // nghìn
    const g0 = groups[b * 3 + 0]; // đơn vị

    const blockHasVal = g2 > 0 || g1 > 0 || g0 > 0;
    if (!blockHasVal) continue;

    if (g2 > 0) {
      const w = readThreeDigits(g2, hasHigherOverall);
      resultParts.push(...w, 'triệu');
      hasHigherOverall = true;
    }

    if (g1 > 0) {
      const w = readThreeDigits(g1, hasHigherOverall);
      resultParts.push(...w, 'nghìn');
      hasHigherOverall = true;
    }

    if (g0 > 0) {
      const w = readThreeDigits(g0, hasHigherOverall);
      resultParts.push(...w);
      hasHigherOverall = true;
    }

    if (b > 0) {
      for (let k = 0; k < b; k++) {
        resultParts.push('tỷ');
      }
    }
  }

  if (resultParts.length === 0) {
    return 'không đồng';
  }

  return `${resultParts.join(' ')} đồng`;
}
