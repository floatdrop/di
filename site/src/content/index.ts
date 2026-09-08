import { en } from './en.tsx';
import { ru } from './ru.tsx';
import type { Content, Locale } from './types.ts';

export const content: Record<Locale, Content> = { en, ru };

/** Each locale's name in its own language, for the language switch. */
export const nativeNames: Record<Locale, string> = {
	en: en.nativeName,
	ru: ru.nativeName
};
