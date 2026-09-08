import { Text } from '../uikit.ts';
import type { ReactNode } from 'react';

/**
 * Inline code inside a sentence. code-inline-3 is 16px against the 17px of
 * body-3, which is the one step that keeps it on the line; code-inline-2 is
 * 14px and reads as a different size of text rather than a different kind.
 */
export function C({ children }: { children: ReactNode }) {
	return (
		<Text as="code" variant="code-inline-3" className="di-code">
			{children}
		</Text>
	);
}
