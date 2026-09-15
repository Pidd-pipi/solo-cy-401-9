import { getData, postData } from './request';
import type {
  ApproveChangeResult,
  Contract,
  ContractChange,
  CreateContractChangePayload
} from '../types';

export const contractApi = {
  list: () => getData<Contract[]>('/contracts'),
  detail: (id: number) => getData<Contract>(`/contracts/${id}`),
  sign: (id: number) => postData<Contract>(`/contracts/${id}/sign`),
  complete: (id: number) => postData<Contract>(`/contracts/${id}/complete`),

  listChanges: (id: number) => getData<ContractChange[]>(`/contracts/${id}/changes`),
  createChange: (id: number, payload: CreateContractChangePayload) =>
    postData<ContractChange>(`/contracts/${id}/changes`, payload),
  approveChange: (id: number, changeId: number) =>
    postData<ApproveChangeResult>(`/contracts/${id}/changes/${changeId}/approve`),
  rejectChange: (id: number, changeId: number) =>
    postData<ContractChange>(`/contracts/${id}/changes/${changeId}/reject`),
  withdrawChange: (id: number, changeId: number) =>
    postData<ContractChange>(`/contracts/${id}/changes/${changeId}/withdraw`)
};
