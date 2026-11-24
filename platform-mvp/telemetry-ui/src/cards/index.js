import { loadCards } from './registry'
import { connections } from './connections'
import { network } from './network'

// The plugins the application page shows, in order. A new card is a file that calls defineCard, and a line here.
export const CARDS = [connections, network]

export const loadServiceCards = (client, id, opts) => loadCards(client, id, { ...opts, cards: CARDS })
