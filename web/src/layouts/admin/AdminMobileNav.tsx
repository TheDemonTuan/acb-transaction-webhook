import React from 'react';
import { NavLink, useNavigate } from 'react-router-dom';
import {
  LayoutDashboard,
  Landmark,
  Bell,
  Activity,
  Server,
  Receipt,
  X,
} from 'lucide-react';

export const AdminMobileNav: React.FC<{ isOpen: boolean; onClose: () => void }> = ({
  isOpen,
  onClose,
}) => {
  const navigate = useNavigate();

  if (!isOpen) return null;

  const navItems = [
    { to: '/admin/overview', label: 'Tổng quan', icon: LayoutDashboard },
    { to: '/admin/connection', label: 'Kết nối payOS', icon: Landmark },
    { to: '/admin/notifications', label: 'Kênh thông báo', icon: Bell },
    { to: '/admin/activity', label: 'Hoạt động', icon: Activity },
    { to: '/admin/system', label: 'Hệ thống', icon: Server },
  ];

  return (
    <div className="fixed inset-0 z-50 lg:hidden flex">
      <div className="fixed inset-0 bg-black/40 backdrop-blur-xs" onClick={onClose} />
      <div className="relative w-64 max-w-[80vw] bg-white h-full flex flex-col z-10 shadow-2xl">
        <div className="p-4 border-b border-stone-100 flex items-center justify-between">
          <div className="flex items-center gap-2">
            <div className="w-7 h-7 rounded-lg bg-stone-900 text-white flex items-center justify-center font-bold text-xs">
              A
            </div>
            <span className="font-bold text-sm text-stone-900">Admin Console</span>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="p-1 rounded-lg text-stone-400 hover:text-stone-600"
          >
            <X className="w-5 h-5" />
          </button>
        </div>

        <nav className="p-4 space-y-1.5 flex-1 overflow-y-auto">
          <button
            type="button"
            role="button"
            onClick={() => {
              onClose();
              navigate('/transactions');
            }}
            className="w-full flex items-center gap-3 px-3.5 py-2.5 rounded-xl text-xs font-semibold text-emerald-800 bg-emerald-50 mb-3"
          >
            <Receipt className="w-4 h-4 text-emerald-600" />
            <span>Giao dịch (Viewer)</span>
          </button>

          {navItems.map((item) => {
            const Icon = item.icon;
            return (
              <NavLink
                key={item.to}
                to={item.to}
                role="button"
                onClick={onClose}
                className={({ isActive }) =>
                  `flex items-center gap-3 px-3.5 py-2.5 rounded-xl text-xs font-medium transition ${
                    isActive ? 'bg-stone-900 text-white font-semibold' : 'text-stone-600 hover:bg-stone-100'
                  }`
                }
              >
                <Icon className="w-4 h-4" />
                <span>{item.label}</span>
              </NavLink>
            );
          })}
        </nav>
      </div>
    </div>
  );
};
