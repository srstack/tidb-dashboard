import React from 'react'
import i18next from 'i18next'
import axios, { AxiosInstance } from 'axios'
import { message, Modal, notification } from 'antd'
import * as singleSpa from 'single-spa'

import { routing, i18n } from '@pingcap/tidb-dashboard-lib'
import {
  Configuration,
  DefaultApi as DashboardApi
} from '@pingcap/tidb-dashboard-client'

import auth from '~/uilts/auth'

import translations from './translations'

export * from '@pingcap/tidb-dashboard-client'

//////////////////////////////

const client = {
  init(apiBasePath: string, apiInstance: DashboardApi) {
    this.apiBasePath = apiBasePath
    this.apiInstance = apiInstance
  },

  getInstance(): DashboardApi {
    return this.apiInstance
  },

  getBasePath(): string {
    return this.apiBasePath
  }
}

export default client

//////////////////////////////

type HandleError = 'default' | 'custom'

function applyErrorHandlerInterceptor(instance: AxiosInstance) {
  instance.interceptors.response.use(undefined, async function (err) {
    const { response, config } = err
    const handleError = config.handleError as HandleError
    const method = (config.method as string).toLowerCase()

    let errCode: string
    let content: string
    if (err.message === 'Network Error') {
      errCode = 'common.network'
    } else {
      errCode = response?.data?.code
    }
    if (i18next.exists(`error.${errCode ?? ''}`)) {
      // If there is a translation for the code, use the translation.
      // TODO: Better to display error details somewhere.
      content = i18next.t(`error.${errCode}`)
    } else {
      content = String(
        response?.data?.message || err.message || 'Internal error'
      )
    }
    err.message = content
    err.errCode = errCode

    if (errCode === 'common.unauthenticated') {
      // Handle unauthorized error in a unified way
      if (!routing.isLocationMatch('/') && !routing.isSignInPage()) {
        message.error({ content, key: errCode })
      }
      auth.clearAuthToken()
      singleSpa.navigateToUrl('#' + routing.signInRoute)
      err.handled = true
    } else if (handleError === 'default') {
      if (method === 'get') {
        const fullUrl = config.url as string
        const API = fullUrl.replace(client.getBasePath(), '').split('?')[0]
        notification.error({
          key: API,
          message: i18next.t('error.title'),
          description: (
            <span>
              API: {API}
              <br />
              {content}
            </span>
          )
        })
      } else if (['post', 'put', 'delete', 'patch'].includes(method)) {
        Modal.error({
          title: i18next.t('error.title'),
          content: content,
          zIndex: 2000 // higher than popover
        })
      }
      err.handled = true
    }

    return Promise.reject(err)
  })
}

function initAxios(token: string) {
  const instance = axios.create({
    headers: {
      'x-csrf-token': token
    }
  })
  applyErrorHandlerInterceptor(instance)

  return instance
}

export function setupClient(apiBasePath: string, token: string) {
  i18n.addTranslations(translations)

  const axiosInstance = initAxios(token)
  const dashboardApi = new DashboardApi(
    new Configuration({
      basePath: apiBasePath,
      baseOptions: {
        handleError: 'default'
      }
    }),
    undefined,
    axiosInstance
  )

  client.init(apiBasePath, dashboardApi)
}
