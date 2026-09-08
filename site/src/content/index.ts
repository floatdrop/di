import { en } from './en.tsx';
import { ja } from './ja.tsx';
import { ru } from './ru.tsx';
import { zh } from './zh.tsx';
import type { Content, Locale } from './types.ts';

export const content: Record<Locale, Content> = { en, ru, zh, ja };

/** Each locale's name in its own language, for the language switch. */
export const nativeNames: Record<Locale, string> = {
	en: en.nativeName,
	ru: ru.nativeName,
	zh: zh.nativeName,
	ja: ja.nativeName
};
