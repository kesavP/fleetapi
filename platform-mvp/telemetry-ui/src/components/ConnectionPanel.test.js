import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import ConnectionPanel from './ConnectionPanel.vue'
import { ApiError } from '../api/client'

const KEY = 'us-read-key-0123456789abcdef'

function fakeSession(initial = null) {
  let value = initial
  return {
    get: vi.fn(() => value),
    set: vi.fn((v) => {
      value = v
    }),
    clear: vi.fn(() => {
      value = null
    }),
  }
}

const clientOf = (impl) => ({ metrics: vi.fn(impl) })

function mountPanel({ session = fakeSession(), client = clientOf(async () => ['a', 'b', 'c']) } = {}) {
  const wrapper = mount(ConnectionPanel, { props: { client, session } })
  return { wrapper, session, client }
}

async function fill(wrapper, project, key) {
  await wrapper.get('#project').setValue(project)
  await wrapper.get('#key').setValue(key)
}

describe('ConnectionPanel', () => {
  it('asks for a project and a read key, and hides the key as it is typed', () => {
    const { wrapper } = mountPanel()
    expect(wrapper.get('#project').exists()).toBe(true)
    expect(wrapper.get('#key').attributes('type')).toBe('password')
    expect(wrapper.get('button[type=submit]').attributes('disabled')).toBeDefined()
  })

  it('labels its fields for assistive technology', () => {
    const { wrapper } = mountPanel()
    expect(wrapper.get('label[for=project]').text()).toBe('Project')
    expect(wrapper.get('label[for=key]').text()).toBe('Read key')
  })

  it('does not try a project name that is not valid', async () => {
    const { wrapper, client } = mountPanel()
    await fill(wrapper, '../other', KEY)
    expect(wrapper.get('#project').attributes('aria-invalid')).toBe('true')
    expect(wrapper.text()).toContain('Lowercase letters, digits and dashes only.')
    expect(wrapper.get('button[type=submit]').attributes('disabled')).toBeDefined()
    await wrapper.get('form').trigger('submit')
    expect(client.metrics).not.toHaveBeenCalled()
  })

  it('connects: stores the session, shows the result and lets go of the key', async () => {
    const { wrapper, session, client } = mountPanel()
    await fill(wrapper, 'gke', KEY)
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(client.metrics).toHaveBeenCalledOnce()
    expect(session.set).toHaveBeenCalledWith({ project: 'gke', key: KEY })
    expect(wrapper.get('[role=status]').text()).toContain('Connected to project gke')
    expect(wrapper.get('[role=status]').text()).toContain('3 metric names')
    expect(wrapper.find('#key').exists()).toBe(false)           // the form, and the key typed into it, are gone
    expect(wrapper.html()).not.toContain(KEY)
    expect(wrapper.emitted('connected')[0][0]).toEqual({ project: 'gke', metrics: 3 })
  })

  it('shows the message of a failure and leaves no wrong key behind', async () => {
    const { wrapper, session } = mountPanel({
      client: clientOf(async () => {
        throw new ApiError('This key cannot read project "gke". Check the key and the project name.', { status: 403, kind: 'auth' })
      }),
    })
    await fill(wrapper, 'gke', 'wrong-key-0123456789')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.get('[role=alert]').text()).toContain('cannot read project "gke"')
    expect(session.get()).toBeNull()
    expect(wrapper.find('[role=status]').exists()).toBe(false)
    expect(wrapper.get('#project').element.value).toBe('gke')     // what was typed stays, so it can be corrected
  })

  it('puts back a session that was there before when the new key does not work', async () => {
    // the form is showing, yet a session exists: another part of the page stored one meanwhile
    const previous = { project: 'old', key: 'old-key-0123456789abcdef' }
    const session = fakeSession()
    const { wrapper } = mountPanel({
      session,
      client: clientOf(async () => {
        throw new ApiError('A read key is required.', { status: 401, kind: 'auth' })
      }),
    })
    session.set(previous)
    await fill(wrapper, 'gke', 'bad-key-0123456789')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(session.get()).toEqual(previous)
  })

  it('tells the page it has disconnected', async () => {
    const { wrapper } = mountPanel({ session: fakeSession({ project: 'gke', key: KEY }) })
    await wrapper.get('button.secondary').trigger('click')
    expect(wrapper.emitted('disconnected')).toHaveLength(1)
  })

  it('disables the button while it is connecting', async () => {
    let release
    const { wrapper } = mountPanel({ client: clientOf(() => new Promise((r) => (release = r))) })
    await fill(wrapper, 'gke', KEY)
    await wrapper.get('form').trigger('submit')
    expect(wrapper.get('button[type=submit]').attributes('disabled')).toBeDefined()
    expect(wrapper.get('button[type=submit]').text()).toBe('Connecting…')
    release([])
    await flushPromises()
  })

  it('disconnects: clears the session and shows the form again', async () => {
    const { wrapper, session } = mountPanel({ session: fakeSession({ project: 'gke', key: KEY }) })
    expect(wrapper.get('[role=status]').text()).toContain('Connected to project gke')
    await wrapper.get('button.secondary').trigger('click')
    expect(session.clear).toHaveBeenCalled()
    expect(session.get()).toBeNull()
    expect(wrapper.find('#key').exists()).toBe(true)
    expect(wrapper.get('#key').element.value).toBe('')
  })

  it('starts connected when a session already exists, without calling the API', () => {
    const { client } = mountPanel({ session: fakeSession({ project: 'gke', key: KEY }) })
    expect(client.metrics).not.toHaveBeenCalled()
  })
})
