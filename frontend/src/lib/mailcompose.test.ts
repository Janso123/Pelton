import { describe, expect, it } from 'vitest'
import { formatAddress, parseAddressList, splitAddressList } from './mailcompose'

describe('parseAddressList', () => {
  it('keeps a comma inside a quoted name', () => {
    expect(parseAddressList('"Doe, John" <j@x>, b@y')).toEqual([
      { name: 'Doe, John', email: 'j@x' },
      { name: '', email: 'b@y' },
    ])
  })

  it('splits on semicolons too', () => {
    expect(parseAddressList('a@x; b@y')).toHaveLength(2)
  })

  it('unescapes quotes in a name', () => {
    expect(parseAddressList('"Say \\"hi\\"" <h@x>')[0].name).toBe('Say "hi"')
  })

  it('does not split inside angle brackets', () => {
    expect(parseAddressList('Ann <a,b@x>')).toEqual([{ name: 'Ann', email: 'a,b@x' }])
  })

  it('rejoins an unquoted name with a comma as one address', () => {
    expect(parseAddressList('Doe, John <j@x>, b@y')).toEqual([
      { name: 'Doe, John', email: 'j@x' },
      { name: '', email: 'b@y' },
    ])
  })

  it('rejoins a name with several commas', () => {
    expect(parseAddressList('Doe, Jr., John <j@x>')).toEqual([{ name: 'Doe, Jr., John', email: 'j@x' }])
  })

  it('does not glue a stray word onto a bare address', () => {
    expect(splitAddressList('bob, a@x')).toEqual(['bob', 'a@x'])
  })

  it('drops empty tokens', () => {
    expect(parseAddressList(' , a@x,, ;')).toEqual([{ name: '', email: 'a@x' }])
  })
})

describe('splitAddressList', () => {
  it('returns trimmed raw tokens', () => {
    expect(splitAddressList('"Doe, John" <j@x> , b@y')).toEqual(['"Doe, John" <j@x>', 'b@y'])
  })
})

describe('formatAddress', () => {
  it('is bare without a name', () => {
    expect(formatAddress({ name: '', email: 'a@x' })).toBe('a@x')
  })

  it('leaves a plain name unquoted', () => {
    expect(formatAddress({ name: 'Ann Lee', email: 'a@x' })).toBe('Ann Lee <a@x>')
  })

  it('quotes a name with special characters', () => {
    expect(formatAddress({ name: 'Doe, John', email: 'j@x' })).toBe('"Doe, John" <j@x>')
  })

  it.each(['Doe, John', 'Say "hi"', 'a;b', 'x<y>', 'a@b', 'f(o)o', 'back\\slash'])('round-trips %s', (name) => {
    expect(parseAddressList(formatAddress({ name, email: 'e@x' }))).toEqual([{ name, email: 'e@x' }])
  })
})
